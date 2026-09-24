package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// fileHasSHA256 reports whether path exists and its content hashes to want
// (case-insensitive). Used to skip a download when the target model is
// already cached.
func fileHasSHA256(path, want string) bool {
	if want == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want)
}

// downloadModel fetches url into dest+".part" (resuming with an HTTP Range
// request if that file already has bytes), verifies its SHA-256 against
// wantSHA256, and renames it to dest. onProgress is called with 0..1 as
// bytes arrive; sizeHint is used as the total when the server does not send
// Content-Length (e.g. no Range support and a chunked response).
func downloadModel(ctx context.Context, client *http.Client, url, dest, wantSHA256 string, sizeHint int64, onProgress func(float64), log *slog.Logger) error {
	part := dest + ".part"
	var offset int64
	if fi, err := os.Stat(part); err == nil {
		offset = fi.Size()
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", part, err)
	}
	defer f.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	hasher := sha256.New()
	switch resp.StatusCode {
	case http.StatusOK:
		// No Range support, or nothing to resume: start over.
		offset = 0
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	case http.StatusPartialContent:
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(hasher, f, offset); err != nil {
			return fmt.Errorf("re-hash resumed bytes: %w", err)
		}
	default:
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}

	total := sizeHint
	if resp.ContentLength >= 0 {
		total = offset + resp.ContentLength
	}

	written := offset
	buf := make([]byte, 256*1024)
	lastReport := time.Now()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			hasher.Write(buf[:n])
			written += int64(n)
			if total > 0 && onProgress != nil && time.Since(lastReport) > 200*time.Millisecond {
				onProgress(float64(written) / float64(total))
				lastReport = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(1)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, wantSHA256) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, wantSHA256)
	}
	if err := f.Close(); err != nil {
		return err
	}
	log.Info("model downloaded", "bytes", written)
	return os.Rename(part, dest)
}

// freeBytes returns free space on the filesystem holding dir.
func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// evictLRU deletes cached *.gguf files in dir, oldest mtime first, skipping
// keep and dest, until at least needed bytes are free (starting from free)
// or there is nothing left to evict. It never deletes the model currently
// being served (keep). free/needed are passed in, rather than statted here,
// so this is testable without a real near-full filesystem.
func evictLRU(dir, keep, dest string, free, needed int64, log *slog.Logger) error {
	if free >= needed {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	type cached struct {
		path  string
		mtime time.Time
		size  int64
	}
	var models []cached
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".gguf") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if p == keep || p == dest {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		models = append(models, cached{p, fi.ModTime(), fi.Size()})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].mtime.Before(models[j].mtime) })

	for _, m := range models {
		if free >= needed {
			break
		}
		if err := os.Remove(m.path); err != nil {
			log.Warn("evict cached model failed", "path", m.path, "err", err)
			continue
		}
		log.Info("evicted cached model", "path", m.path, "bytes", m.size)
		free += m.size
	}
	if free < needed {
		return fmt.Errorf("not enough storage: need %d MiB, have %d MiB free", needed/mib, free/mib)
	}
	return nil
}
