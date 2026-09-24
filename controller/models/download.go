package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// errPermanent marks download errors that a retry cannot fix.
var errPermanent = errors.New("permanent")

// startLocked starts downloading e in the background. Call with mu held.
func (c *Catalog) startLocked(e *entry) {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.written.Store(0)
	e.total.Store(0)
	c.wg.Add(1)
	go c.run(ctx, e)
}

// run downloads e's file into <id>.gguf.part (resuming what is there),
// checks it is a GGUF file, hashes it and renames it to <id>.gguf.
func (c *Catalog) run(ctx context.Context, e *entry) {
	defer c.wg.Done()
	id := e.m.ID
	src, err := ResolveSource(e.m.Source, c.opts.HuggingFaceBase)
	if err == nil {
		for attempt := 1; ; attempt++ {
			err = c.fetch(ctx, e, src)
			if err == nil || ctx.Err() != nil || errors.Is(err, errPermanent) || attempt == downloadAttempts {
				break
			}
			c.log.Warn("model download failed, retrying", "model_id", id, "attempt", attempt, "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(c.opts.RetryDelay):
			}
		}
	}
	if ctx.Err() != nil {
		return // removed, or shutting down: the partial file is resumed on the next start
	}
	var sum string
	var size int64
	var meta Meta
	if err == nil {
		sum, size, err = hashFile(c.partPath(id))
	}
	if err == nil {
		if meta, err = ReadMeta(c.partPath(id)); err != nil {
			_ = os.Remove(c.partPath(id)) // not a model: do not resume from it
		}
	}

	c.mu.Lock()
	if c.models[id] != e {
		c.mu.Unlock()
		return // removed while hashing
	}
	e.cancel = nil
	if err == nil {
		err = os.Rename(c.partPath(id), c.filePath(id))
	}
	if err != nil {
		e.m.Status, e.m.Error = StatusError, strings.TrimPrefix(err.Error(), errPermanent.Error()+": ")
	} else {
		m := &e.m
		m.Status, m.Error, m.SizeBytes, m.SHA256 = StatusReady, "", size, sum
		m.Arch, m.Params, m.Quant, m.License = meta.Arch, meta.Params, meta.Quant, meta.License
		m.CtxTrain, m.Layers, m.KVHeads, m.HeadDim = meta.CtxTrain, meta.Layers, meta.KVHeads, meta.HeadDim
		m.SparseBytes, m.ResidentBytes = meta.SparseBytes, size-meta.SparseBytes
		est := m.PlanModel().EstRAM()
		m.EstRAMBytes16k, m.FitsClasses = est, FitsClasses(est)
	}
	_ = c.saveLocked()
	m := e.view()
	c.mu.Unlock()

	if err != nil {
		c.log.Error("model download failed", "model_id", id, "source", m.Source, "err", m.Error)
	} else {
		c.log.Info("model ready", "model_id", id, "size_bytes", m.SizeBytes, "sha256", m.SHA256, "arch", m.Arch,
			"params", m.Params, "quant", m.Quant, "est_ram_bytes_16k", m.EstRAMBytes16k, "fits_classes", m.FitsClasses)
	}
	c.changed()
}

// fetch runs one download attempt, appending to the partial file when the
// source supports it.
func (c *Catalog) fetch(ctx context.Context, e *entry, src Source) error {
	f, err := os.OpenFile(c.partPath(e.m.ID), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("%w: %v", errPermanent, err)
	}
	defer f.Close()
	off, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	restart := func() error {
		off = 0
		if err := f.Truncate(0); err != nil {
			return err
		}
		_, err := f.Seek(0, io.SeekStart)
		return err
	}

	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(c.opts.IdleTimeout, cancel)
	defer idle.Stop()

	var body io.Reader
	var total int64
	if src.Path != "" {
		in, err := os.Open(src.Path)
		if err != nil {
			return fmt.Errorf("%w: %v", errPermanent, err)
		}
		defer in.Close()
		st, err := in.Stat()
		if err != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", errPermanent, src.Path)
		}
		if off > st.Size() {
			if err := restart(); err != nil {
				return err
			}
		}
		if _, err := in.Seek(off, io.SeekStart); err != nil {
			return err
		}
		body, total = in, st.Size()
	} else {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
		if err != nil {
			return fmt.Errorf("%w: %v", errPermanent, err)
		}
		if off > 0 {
			req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-")
		}
		resp, err := c.opts.HTTPClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusPartialContent && off > 0 && rangeStart(resp.Header.Get("Content-Range")) == off:
			c.log.Info("resuming model download", "model_id", e.m.ID, "offset", off)
		case resp.StatusCode == http.StatusOK:
			if err := restart(); err != nil { // the server ignored Range
				return err
			}
		case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable || resp.StatusCode == http.StatusPartialContent:
			if err := restart(); err != nil {
				return err
			}
			return fmt.Errorf("%s: cannot resume at byte %d, restarting", resp.Status, off)
		default:
			err := fmt.Errorf("GET %s: %s", src.URL, resp.Status)
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
				err = fmt.Errorf("%w: %v", errPermanent, err)
			}
			return err
		}
		if resp.ContentLength >= 0 {
			total = off + resp.ContentLength
		}
		body = resp.Body
	}

	e.written.Store(off)
	e.total.Store(total)
	buf := make([]byte, 1<<20)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return fmt.Errorf("%w: %v", errPermanent, err) // disk full, most likely
			}
			e.written.Add(int64(n))
			idle.Reset(c.opts.IdleTimeout)
		}
		if rerr == io.EOF {
			break
		}
		if rerr == nil && ctx.Err() != nil {
			rerr = ctx.Err() // a local file does not notice the context
		}
		if rerr != nil {
			if ctx.Err() != nil && parent.Err() == nil {
				return fmt.Errorf("no data for %s", c.opts.IdleTimeout)
			}
			return rerr
		}
	}
	if got := e.written.Load(); total > 0 && got != total {
		return fmt.Errorf("download ended at %d of %d bytes", got, total)
	}
	return f.Sync()
}

// rangeStart parses the first byte of "bytes 100-199/200"; -1 if invalid.
func rangeStart(cr string) int64 {
	s, ok := strings.CutPrefix(cr, "bytes ")
	if !ok {
		return -1
	}
	s, _, ok = strings.Cut(s, "-")
	n, err := strconv.ParseInt(s, 10, 64)
	if !ok || err != nil {
		return -1
	}
	return n
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
