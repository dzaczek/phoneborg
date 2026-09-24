package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestFileHasSHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.gguf")
	content := []byte("model bytes")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if !fileHasSHA256(path, sha256Hex(content)) {
		t.Error("expected matching hash to report true")
	}
	if fileHasSHA256(path, "deadbeef") {
		t.Error("expected mismatched hash to report false")
	}
	if fileHasSHA256(filepath.Join(dir, "missing.gguf"), sha256Hex(content)) {
		t.Error("expected missing file to report false")
	}
	if fileHasSHA256(path, "") {
		t.Error("expected empty want to report false")
	}
}

func TestDownloadModelFresh(t *testing.T) {
	content := []byte("0123456789abcdefghij")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "model.gguf")
	var progress []float64
	err := downloadModel(context.Background(), srv.Client(), srv.URL, dest, sha256Hex(content), int64(len(content)),
		func(p float64) { progress = append(progress, p) }, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("content = %q, want %q", got, content)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error(".part file should be gone after a successful download")
	}
	if len(progress) == 0 || progress[len(progress)-1] != 1 {
		t.Fatalf("progress = %v, want to end at 1", progress)
	}
}

func TestDownloadModelResumes(t *testing.T) {
	content := []byte("the quick brown fox jumps over the lazy dog, twice over")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Write(content)
			return
		}
		var start int
		if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err != nil {
			t.Errorf("bad Range header %q: %v", rng, err)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content[start:])
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "model.gguf")
	split := 20
	if err := os.WriteFile(dest+".part", content[:split], 0o644); err != nil {
		t.Fatal(err)
	}

	if err := downloadModel(context.Background(), srv.Client(), srv.URL, dest, sha256Hex(content), int64(len(content)), nil, discardLog()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("content = %q, want %q", got, content)
	}
}

func TestDownloadModelSHAMismatch(t *testing.T) {
	content := []byte("some model bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "model.gguf")
	err := downloadModel(context.Background(), srv.Client(), srv.URL, dest, "0000000000000000000000000000000000000000000000000000000000000000", int64(len(content)), nil, discardLog())
	if err == nil {
		t.Fatal("expected a sha256 mismatch error")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Error("dest should not exist after a hash mismatch")
	}
}

func TestDownloadModelHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "model.gguf")
	if err := downloadModel(context.Background(), srv.Client(), srv.URL, dest, "abc", 10, nil, discardLog()); err == nil {
		t.Fatal("expected an error for a non-200/206 response")
	}
}

func TestEvictLRU(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		mtime := time.Now().Add(-age)
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldest := write("old.gguf", 100, 3*time.Hour)
	middle := write("mid.gguf", 100, 2*time.Hour)
	serving := write("serving.gguf", 100, 1*time.Hour)

	t.Run("enough free space evicts nothing", func(t *testing.T) {
		if err := evictLRU(dir, serving, "", 1000, 100, discardLog()); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{oldest, middle, serving} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s should still exist: %v", p, err)
			}
		}
	})

	t.Run("evicts oldest first, never the served model", func(t *testing.T) {
		// free=0, need 150: evicting "old.gguf" (100 bytes) is not enough,
		// must also evict "mid.gguf" to reach 200 >= 150.
		if err := evictLRU(dir, serving, "", 0, 150, discardLog()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(oldest); !os.IsNotExist(err) {
			t.Error("oldest cached model should have been evicted")
		}
		if _, err := os.Stat(middle); !os.IsNotExist(err) {
			t.Error("mid cached model should have been evicted")
		}
		if _, err := os.Stat(serving); err != nil {
			t.Error("the served model must never be evicted")
		}
	})

	t.Run("errors when even evicting everything is not enough", func(t *testing.T) {
		err := evictLRU(dir, serving, "", 0, 100000, discardLog())
		if err == nil {
			t.Fatal("expected an error when nothing fits")
		}
	})
}
