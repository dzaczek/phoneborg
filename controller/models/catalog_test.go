package models

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller/models/modeltest"
)

var testFile = modeltest.GGUF(modeltest.LlamaLike("qwen2", 28, 12, 2, 1536, 32768), 3<<20)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// hfServer serves testFile at /<owner>/<repo>/resolve/main/<file>, with
// Range support, and records the Range headers it saw.
type hfServer struct {
	*httptest.Server
	mu     sync.Mutex
	ranges []string
	hits   atomic.Int32
}

func newHF(t *testing.T, h func(s *hfServer, w http.ResponseWriter, r *http.Request)) *hfServer {
	s := &hfServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.mu.Lock()
		s.ranges = append(s.ranges, r.Header.Get("Range"))
		s.mu.Unlock()
		if h != nil {
			h(s, w, r)
			return
		}
		serveTestFile(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func serveTestFile(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/resolve/main/Qwen2.5-1.5B-Instruct-Q4_K_M.gguf") {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, "m.gguf", time.Time{}, bytes.NewReader(testFile))
}

func newCatalog(t *testing.T, dir string, hf *hfServer) (*Catalog, chan struct{}) {
	t.Helper()
	changed := make(chan struct{}, 100)
	opts := CatalogOptions{Dir: filepath.Join(dir, "models"), StateFile: filepath.Join(dir, "models.json"),
		RetryDelay: 10 * time.Millisecond, IdleTimeout: 2 * time.Second, OnChange: func() { changed <- struct{}{} }}
	if hf != nil {
		opts.HuggingFaceBase = hf.URL
	}
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, changed
}

// wait returns the model once it is no longer downloading.
func wait(t *testing.T, c *Catalog, id string) Model {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m, ok := c.Get(id); ok && m.Status != StatusDownloading {
			return m
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("model %s still downloading", id)
	return Model{}
}

const hfSource = "hf://Qwen/Qwen2.5-1.5B-Instruct-GGUF/Qwen2.5-1.5B-Instruct-Q4_K_M.gguf"

func TestCatalogDownloadFromHuggingFace(t *testing.T) {
	dir := t.TempDir()
	hf := newHF(t, nil)
	c, _ := newCatalog(t, dir, hf)
	m, err := c.Add(AddRequest{Source: hfSource, Tags: []string{"Chat", " chat", "small"}, RecommendedClasses: []string{"m", "s"}, RecommendedTiers: []string{"t3", "T2", "t2"}})
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "qwen2.5-1.5b-instruct-q4_k_m" || m.Status != StatusDownloading || m.Name != "Qwen2.5-1.5B-Instruct-Q4_K_M" {
		t.Fatalf("added = %+v", m)
	}
	m = wait(t, c, m.ID)
	if m.Status != StatusReady || m.Error != "" {
		t.Fatalf("model = %+v", m)
	}
	if m.SHA256 != sum(testFile) || m.SizeBytes != int64(len(testFile)) || m.Progress != 1 || m.File != "Qwen2.5-1.5B-Instruct-Q4_K_M.gguf" {
		t.Fatalf("file fields = %+v", m)
	}
	if m.Arch != "qwen2" || m.Params != "1.5B" || m.Quant != "Q4_K_M" || m.CtxTrain != 32768 || m.Layers != 28 ||
		m.KVHeads != 2 || m.HeadDim != 128 || m.License != "apache-2.0" {
		t.Fatalf("metadata = %+v", m)
	}
	if want := EstimateRAM(m.SizeBytes, 28, 2, 128, 16384); m.EstRAMBytes16k != want || strings.Join(m.FitsClasses, ",") != "s,m,l,xl" {
		t.Fatalf("estimate = %d %v, want %d", m.EstRAMBytes16k, m.FitsClasses, want)
	}
	if strings.Join(m.Tags, ",") != "chat,small" || strings.Join(m.RecommendedClasses, ",") != "s,m" {
		t.Fatalf("tags = %v classes = %v", m.Tags, m.RecommendedClasses)
	}
	// Deduplicated case-insensitively and ordered t1..t4, like classes.
	if strings.Join(m.RecommendedTiers, ",") != "t2,t3" {
		t.Fatalf("tiers = %v", m.RecommendedTiers)
	}
	path, _, ok := c.File(m.ID)
	if data, _ := os.ReadFile(path); !ok || !bytes.Equal(data, testFile) {
		t.Fatalf("file %s: %v", path, ok)
	}
	if _, err := os.Stat(path + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial file left behind")
	}
	if _, err := c.Add(AddRequest{Source: hfSource}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate add: %v", err)
	}

	// A new controller finds the model again.
	c.Close()
	c2, _ := newCatalog(t, dir, hf)
	if m2, ok := c2.Get(m.ID); !ok || m2.Status != StatusReady || m2.SHA256 != m.SHA256 || m2.Progress != 1 {
		t.Fatalf("reloaded = %+v", m2)
	}
}

func TestCatalogResumesInterruptedDownload(t *testing.T) {
	cut := len(testFile) / 2
	hf := newHF(t, func(s *hfServer, w http.ResponseWriter, r *http.Request) {
		if s.hits.Load() == 1 { // first attempt: the connection drops halfway
			w.Header().Set("Content-Length", "999999999")
			w.Write(testFile[:cut])
			panic(http.ErrAbortHandler)
		}
		serveTestFile(w, r)
	})
	c, _ := newCatalog(t, t.TempDir(), hf)
	m, err := c.Add(AddRequest{Source: hf.URL + "/Qwen/r/resolve/main/Qwen2.5-1.5B-Instruct-Q4_K_M.gguf", ID: "q"})
	if err != nil {
		t.Fatal(err)
	}
	m = wait(t, c, m.ID)
	if m.Status != StatusReady || m.SHA256 != sum(testFile) {
		t.Fatalf("model = %+v", m)
	}
	hf.mu.Lock()
	defer hf.mu.Unlock()
	if len(hf.ranges) != 2 || hf.ranges[0] != "" || !strings.HasPrefix(hf.ranges[1], "bytes=") || hf.ranges[1] == "bytes=0-" {
		t.Fatalf("ranges = %q", hf.ranges)
	}
}

func TestCatalogResumesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	hf := newHF(t, nil)
	// A previous controller stopped mid-download.
	os.MkdirAll(filepath.Join(dir, "models"), 0o700)
	os.WriteFile(filepath.Join(dir, "models", "q.gguf.part"), testFile[:1000], 0o600)
	os.WriteFile(filepath.Join(dir, "models.json"), []byte(`{"version":1,"models":[{"id":"q","name":"q","source":"`+hfSource+`","status":"downloading"}]}`), 0o600)
	c, _ := newCatalog(t, dir, hf)
	if m := wait(t, c, "q"); m.Status != StatusReady || m.SHA256 != sum(testFile) {
		t.Fatalf("model = %+v", m)
	}
	if hf.ranges[0] != "bytes=1000-" {
		t.Fatalf("ranges = %q", hf.ranges)
	}
}

func TestCatalogDownloadErrors(t *testing.T) {
	notGGUF := newHF(t, func(_ *hfServer, w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html>login</html>")) })
	missing := newHF(t, func(_ *hfServer, w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	flaky := newHF(t, func(_ *hfServer, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	c, _ := newCatalog(t, t.TempDir(), nil)
	for _, tc := range []struct {
		srv   *hfServer
		want  string
		tries int32
	}{
		{notGGUF, "not a readable GGUF file", 1},
		{missing, "404 Not Found", 1}, // permanent: no retry
		{flaky, "502 Bad Gateway", 3}, // retried
	} {
		m, err := c.Add(AddRequest{Source: tc.srv.URL + "/x.gguf"})
		if err != nil {
			t.Fatal(err)
		}
		m = wait(t, c, m.ID)
		if m.Status != StatusError || !strings.Contains(m.Error, tc.want) || tc.srv.hits.Load() != tc.tries {
			t.Errorf("%s: model = %+v, hits %d", tc.want, m, tc.srv.hits.Load())
		}
		if _, err := os.Stat(filepath.Join(c.Dir(), "x.gguf")); err == nil {
			t.Errorf("%s: file kept", tc.want)
		}
		if _, _, ok := c.File("x"); ok {
			t.Errorf("%s: failed model is servable", tc.want)
		}
		if err := c.Remove("x"); err != nil {
			t.Fatal(err)
		}
	}
	// A failed model can be added again (retried).
	m, _ := c.Add(AddRequest{Source: missing.URL + "/x.gguf"})
	wait(t, c, m.ID)
	if _, err := c.Add(AddRequest{Source: missing.URL + "/x.gguf"}); err != nil {
		t.Fatalf("retry after error: %v", err)
	}
	wait(t, c, m.ID)
}

// TestCatalogComputesSparseAndResidentBytes covers ADR-012's addendum: a
// downloaded model's sparse (per_layer_token_embd.weight) tensor bytes are
// split out of size_bytes into resident_bytes.
func TestCatalogComputesSparseAndResidentBytes(t *testing.T) {
	const sparseElements = 1024
	data := modeltest.GGUFTensors(modeltest.LlamaLike("gemma3n", 4, 8, 2, 256, 32768), []modeltest.Tensor{
		{Name: "token_embd.weight", Elements: 8},
		{Name: "per_layer_token_embd.weight", Elements: sparseElements},
	}, 1<<20)
	dir := t.TempDir()
	src := filepath.Join(dir, "sparse.gguf")
	os.WriteFile(src, data, 0o600)
	c, _ := newCatalog(t, dir, nil)
	m, err := c.Add(AddRequest{Source: "file://" + src})
	if err != nil {
		t.Fatal(err)
	}
	m = wait(t, c, m.ID)
	if m.Status != StatusReady || m.Error != "" {
		t.Fatalf("model = %+v", m)
	}
	wantSparse := int64(sparseElements * 4)
	if m.SparseBytes != wantSparse {
		t.Fatalf("SparseBytes = %d, want %d", m.SparseBytes, wantSparse)
	}
	if want := m.SizeBytes - wantSparse; m.ResidentBytes != want {
		t.Fatalf("ResidentBytes = %d, want %d", m.ResidentBytes, want)
	}
}

// TestCatalogRecomputesSparseBytesOnLoad covers ADR-012's addendum
// requirement that existing (already downloaded) catalog entries get
// sparse_bytes/resident_bytes computed lazily, without re-downloading, so an
// upgraded controller does not keep treating a Gemma-3n-style model as
// needing its whole file resident.
func TestCatalogRecomputesSparseBytesOnLoad(t *testing.T) {
	dir := t.TempDir()
	data := modeltest.GGUFTensors(modeltest.LlamaLike("gemma3n", 4, 8, 2, 256, 32768), []modeltest.Tensor{
		{Name: "token_embd.weight", Elements: 8},
		{Name: "per_layer_token_embd.weight", Elements: 1024},
	}, 0)
	os.MkdirAll(filepath.Join(dir, "models"), 0o700)
	os.WriteFile(filepath.Join(dir, "models", "old.gguf"), data, 0o600)
	// A catalog file saved before sparse_bytes/resident_bytes existed: ready,
	// with size_bytes set, but no sparse_bytes/resident_bytes fields.
	state := fmt.Sprintf(`{"version":1,"models":[{"id":"old","name":"old","source":"file:///old.gguf","status":"ready","size_bytes":%d}]}`, len(data))
	os.WriteFile(filepath.Join(dir, "models.json"), []byte(state), 0o600)

	c, _ := newCatalog(t, dir, nil)
	m, ok := c.Get("old")
	if !ok {
		t.Fatal("model not loaded")
	}
	if want := int64(1024 * 4); m.SparseBytes != want {
		t.Fatalf("SparseBytes = %d, want %d (recomputed on load)", m.SparseBytes, want)
	}
	if want := m.SizeBytes - int64(1024*4); m.ResidentBytes != want {
		t.Fatalf("ResidentBytes = %d, want %d", m.ResidentBytes, want)
	}

	// Persisted, so a second restart does not need to recompute again.
	c.Close()
	raw, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved catalogFile
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Models) != 1 || saved.Models[0].ResidentBytes != m.ResidentBytes || saved.Models[0].SparseBytes != m.SparseBytes {
		t.Fatalf("saved models = %+v, want ResidentBytes %d SparseBytes %d", saved.Models, m.ResidentBytes, m.SparseBytes)
	}
}

func TestCatalogFileSourceAndRemove(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Local-Model.gguf")
	os.WriteFile(src, testFile, 0o600)
	c, changed := newCatalog(t, dir, nil)
	m, err := c.Add(AddRequest{Source: "file://" + src, Name: "Local"})
	if err != nil {
		t.Fatal(err)
	}
	if m = wait(t, c, "local-model"); m.Status != StatusReady || m.SHA256 != sum(testFile) || m.Name != "Local" {
		t.Fatalf("model = %+v", m)
	}
	name := "Renamed"
	if m, err = c.Update("local-model", Patch{Name: &name, Tags: []string{}, RecommendedClasses: []string{"xl"}, RecommendedTiers: []string{"t4"}}); err != nil ||
		m.Name != "Renamed" || len(m.RecommendedClasses) != 1 || strings.Join(m.RecommendedTiers, ",") != "t4" {
		t.Fatalf("update = %+v %v", m, err)
	}
	if _, err := c.Update("local-model", Patch{RecommendedClasses: []string{"huge"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad class: %v", err)
	}
	if _, err := c.Update("local-model", Patch{RecommendedTiers: []string{"t9"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad tier: %v", err)
	}
	// A nil RecommendedTiers (not passed) leaves the tier set from above alone.
	if m, err = c.Update("local-model", Patch{Name: &name}); err != nil || strings.Join(m.RecommendedTiers, ",") != "t4" {
		t.Fatalf("update without tiers = %+v %v", m, err)
	}
	path, _, _ := c.File("local-model")
	if err := c.Remove("local-model"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("file not removed")
	}
	if err := c.Remove("local-model"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("second remove: %v", err)
	}
	if len(changed) < 3 { // added, ready, removed
		t.Fatalf("OnChange called %d times", len(changed))
	}
}

func TestCatalogRemoveStopsDownload(t *testing.T) {
	release := make(chan struct{})
	hf := newHF(t, func(_ *hfServer, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.Write(make([]byte, 1000))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	c, _ := newCatalog(t, t.TempDir(), hf)
	m, _ := c.Add(AddRequest{Source: hf.URL + "/slow.gguf"})
	deadline := time.Now().Add(5 * time.Second)
	for m.Progress == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		m, _ = c.Get("slow")
	}
	if m.Progress <= 0 || m.Progress >= 1 {
		t.Fatalf("progress = %v", m.Progress)
	}
	if err := c.Remove("slow"); err != nil {
		t.Fatal(err)
	}
	c.Close() // waits for the download goroutine
	if entries, _ := os.ReadDir(c.Dir()); len(entries) != 0 {
		t.Fatalf("files left: %v", entries)
	}
}

func TestCatalogRejectsBadRequests(t *testing.T) {
	c, _ := newCatalog(t, t.TempDir(), nil)
	for _, req := range []AddRequest{
		{Source: "ftp://x/y.gguf"},
		{Source: "https://example.com/model.bin"},
		{Source: "hf://owner/model.gguf"},
		{Source: "file://relative/x.gguf"},
		{Source: "https://example.com/big-00001-of-00003.gguf"},
		{Source: "https://example.com/x.gguf", ID: "Bad ID"},
		{Source: "https://example.com/x.gguf", Tags: []string{"a,b"}},
		{Source: "https://example.com/x.gguf", RecommendedClasses: []string{"xxl"}},
		{Source: "https://example.com/x.gguf", RecommendedTiers: []string{"t9"}},
	} {
		if _, err := c.Add(req); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: err = %v", req, err)
		}
	}
	if len(c.List()) != 0 {
		t.Fatal("rejected model was added")
	}
}

func TestResolveSourceAndSlug(t *testing.T) {
	s, err := ResolveSource("hf://bartowski/Llama-3.2-1B-Instruct-GGUF/Llama-3.2-1B-Instruct-Q4_K_M.gguf", HuggingFaceBase)
	if err != nil || s.URL != "https://huggingface.co/bartowski/Llama-3.2-1B-Instruct-GGUF/resolve/main/Llama-3.2-1B-Instruct-Q4_K_M.gguf" ||
		s.File != "Llama-3.2-1B-Instruct-Q4_K_M.gguf" {
		t.Fatalf("hf = %+v %v", s, err)
	}
	if s, err := ResolveSource("file:///srv/models/a.gguf", ""); err != nil || s.Path != "/srv/models/a.gguf" {
		t.Fatalf("file = %+v %v", s, err)
	}
	for in, want := range map[string]string{
		"qwen2.5-0.5b-instruct-q4_k_m.gguf": "qwen2.5-0.5b-instruct-q4_k_m",
		"Qwen2.5-0.5B-Instruct-Q4_K_M.gguf": "qwen2.5-0.5b-instruct-q4_k_m",
		"gemma 3 (1b) it.GGUF":              "gemma-3--1b--it",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}
