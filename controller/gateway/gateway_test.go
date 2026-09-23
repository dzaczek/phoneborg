package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeLlama mimics llama-server's chat completion response.
func fakeLlama(t *testing.T, name string) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Stream bool }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi from %s\"}}]}\n\n", name)
			w.(http.Flusher).Flush()
			fmt.Fprint(w, "data: {\"choices\":[],\"timings\":{\"prompt_n\":5,\"prompt_per_second\":100,\"predicted_n\":7,\"predicted_per_second\":42}}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"hi from %s"}}],"usage":{"prompt_tokens":5,"completion_tokens":7},"timings":{"predicted_per_second":42}}`, name)
	}))
	t.Cleanup(s.Close)
	return s
}

func newGW(t *testing.T, auth Authenticator, backends ...Backend) (*Gateway, http.Handler) {
	g := New(auth, &LeastInflight{}, func() []Backend { return backends },
		Config{UpstreamTimeout: 5 * time.Second, MaxAttempts: 2, Cooldown: time.Minute},
		prometheus.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	g.Register(mux)
	return g, mux
}

func post(h http.Handler, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const chat = `{"model":"m","messages":[{"role":"user","content":"x"}]}`

func TestSpreadsLoadAcrossNodes(t *testing.T) {
	a, b := fakeLlama(t, "a"), fakeLlama(t, "b")
	g, h := newGW(t, AllowAll{}, Backend{NodeID: "a", Model: "m", URL: a.URL}, Backend{NodeID: "b", Model: "m", URL: b.URL})
	for i := 0; i < 10; i++ {
		if w := post(h, chat); w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
	}
	for _, n := range []string{"a", "b"} {
		if got := testutil.ToFloat64(g.mRequests.WithLabelValues("m", n, "200", "anonymous")); got != 5 {
			t.Errorf("node %s served %v requests, want 5", n, got)
		}
	}
	if got := testutil.ToFloat64(g.mTokens.WithLabelValues("m", "a", "completion", "anonymous")); got != 35 {
		t.Errorf("completion tokens = %v", got)
	}
	if got := testutil.ToFloat64(g.mGenTPS.WithLabelValues("a")); got != 42 {
		t.Errorf("tps = %v", got)
	}
}

func TestFailoverToHealthyNode(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // connection refused
	loading := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"Loading model"}`, http.StatusServiceUnavailable)
	}))
	defer loading.Close()
	live := fakeLlama(t, "live")

	for name, bad := range map[string]string{"refused": deadURL, "503": loading.URL} {
		g, h := newGW(t, AllowAll{}, Backend{NodeID: "bad", Model: "m", URL: bad}, Backend{NodeID: "live", Model: "m", URL: live.URL})
		for i := 0; i < 4; i++ {
			w := post(h, chat)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "hi from live") {
				t.Fatalf("%s: status %d body %s", name, w.Code, w.Body)
			}
		}
		// After one failure the bad node is in cooldown and not retried.
		if got := testutil.ToFloat64(g.mUpstream.WithLabelValues("bad")); got != 1 {
			t.Errorf("%s: upstream errors = %v, want 1", name, got)
		}
	}
}

func TestAllBackendsDown(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	_, h := newGW(t, AllowAll{}, Backend{NodeID: "a", Model: "m", URL: dead.URL})
	w := post(h, chat)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"backends_failed"`) {
		t.Fatalf("status %d body %s", w.Code, w.Body)
	}
}

func TestModelRouting(t *testing.T) {
	a, b := fakeLlama(t, "a"), fakeLlama(t, "b")
	_, h := newGW(t, AllowAll{}, Backend{NodeID: "a", Model: "small", URL: a.URL}, Backend{NodeID: "b", Model: "big", URL: b.URL})
	if w := post(h, `{"model":"big"}`); !strings.Contains(w.Body.String(), "hi from b") {
		t.Fatalf("big routed wrong: %s", w.Body)
	}
	if w := post(h, `{"model":"nope"}`); w.Code != 404 {
		t.Fatalf("unknown model: %d", w.Code)
	}
	_, empty := newGW(t, AllowAll{})
	if w := post(empty, chat); w.Code != 503 {
		t.Fatalf("no nodes: %d", w.Code)
	}
}

func TestStreamingPassthrough(t *testing.T) {
	a := fakeLlama(t, "a")
	g, h := newGW(t, AllowAll{}, Backend{NodeID: "a", Model: "m", URL: a.URL})
	w := post(h, `{"model":"m","stream":true}`)
	if w.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("stream not passed through: %q %s", w.Header(), w.Body)
	}
	if got := testutil.ToFloat64(g.mTokens.WithLabelValues("m", "a", "completion", "anonymous")); got != 7 {
		t.Errorf("stream completion tokens = %v", got)
	}
}

func TestStaticKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys")
	os.WriteFile(path, []byte("# comment\nalice sk-alice-123\n\nbob sk-bob-456\n"), 0o600)
	keys, err := LoadKeysFile(path)
	if err != nil {
		t.Fatal(err)
	}
	a := fakeLlama(t, "a")
	g, h := newGW(t, keys, Backend{NodeID: "a", Model: "m", URL: a.URL})
	if w := post(h, chat); w.Code != 401 {
		t.Fatalf("no key: %d", w.Code)
	}
	if w := post(h, chat, "Authorization", "Bearer wrong"); w.Code != 401 {
		t.Fatalf("wrong key: %d", w.Code)
	}
	if w := post(h, chat, "Authorization", "Bearer sk-bob-456"); w.Code != 200 {
		t.Fatalf("bearer: %d", w.Code)
	}
	if w := post(h, chat, "x-api-key", "sk-alice-123"); w.Code != 200 {
		t.Fatalf("x-api-key: %d", w.Code)
	}
	if got := testutil.ToFloat64(g.mRequests.WithLabelValues("m", "a", "200", "bob")); got != 1 {
		t.Errorf("principal label: %v", got)
	}
	if got := testutil.ToFloat64(g.mTokens.WithLabelValues("m", "a", "prompt", "alice")); got != 5 {
		t.Errorf("alice prompt tokens: %v", got)
	}
}

func TestModelsList(t *testing.T) {
	_, h := newGW(t, AllowAll{}, Backend{NodeID: "a", Model: "m1", URL: "x"}, Backend{NodeID: "b", Model: "m1", URL: "y"}, Backend{NodeID: "c", Model: "m2", URL: "z"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var out struct {
		Data []struct {
			ID    string
			Nodes int
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Data) != 2 || out.Data[0].ID != "m1" || out.Data[0].Nodes != 2 {
		t.Fatalf("models: %s", w.Body)
	}
}

func TestReapRetriesRequestStuckOnLostNode(t *testing.T) {
	stop := make(chan struct{})
	frozen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // never answers, like a paused phone
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer frozen.Close()
	defer close(stop)
	live := fakeLlama(t, "live")

	var mu sync.Mutex
	set := []Backend{{NodeID: "frozen", Model: "m", URL: frozen.URL}}
	g := New(AllowAll{}, &LeastInflight{}, func() []Backend { mu.Lock(); defer mu.Unlock(); return set },
		Config{UpstreamTimeout: time.Minute, MaxAttempts: 2, Cooldown: time.Minute},
		prometheus.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	g.Register(mux)

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- post(mux, chat) }()
	for g.inflightOf("frozen") == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	// Node goes SUSPECT; a healthy one is available.
	mu.Lock()
	set = []Backend{{NodeID: "live", Model: "m", URL: live.URL}}
	mu.Unlock()
	g.Reap()

	select {
	case w := <-done:
		if w.Code != 200 || !strings.Contains(w.Body.String(), "hi from live") {
			t.Fatalf("status %d body %s", w.Code, w.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request still stuck on lost node")
	}
}
