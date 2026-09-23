package gateway

import (
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
)

func TestParseKeyLine(t *testing.T) {
	h := hashKey("sk-1")
	for _, tc := range []struct {
		line    string
		want    KeyEntry
		skip    bool
		wantErr bool
	}{
		{line: "  # comment", skip: true},
		{line: "", skip: true},
		{line: "alice sk-1", want: KeyEntry{Name: "alice", Hash: h}},
		{line: "alice sha256:" + h, want: KeyEntry{Name: "alice", Hash: h}},
		{line: "alice sha256:" + strings.ToUpper(h), want: KeyEntry{Name: "alice", Hash: h}},
		{line: "alice sha256:" + h + " 2026-01-02T03:04:05Z", want: KeyEntry{Name: "alice", Hash: h, Created: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}},
		{line: "alice", wantErr: true},
		{line: "alice sk-1 extra", wantErr: true},
		{line: "alice sha256:abc", wantErr: true},
		{line: "alice sha256:" + h + " yesterday", wantErr: true},
		{line: "alice sha256:" + h + " 2026-01-02T03:04:05Z x", wantErr: true},
	} {
		got, ok, err := parseKeyLine(tc.line)
		if (err != nil) != tc.wantErr || ok == (tc.skip || tc.wantErr) || (ok && (got.Name != tc.want.Name || got.Hash != tc.want.Hash || !got.Created.Equal(tc.want.Created))) {
			t.Errorf("%q: got %+v ok=%v err=%v", tc.line, got, ok, err)
		}
	}
}

func authAs(t *testing.T, a Authenticator, key string) (string, error) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	p, err := a.Authenticate(r)
	return p.Name, err
}

func TestKeyLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys")
	os.WriteFile(path, []byte("legacy sk-legacy\n"), 0o600)
	keys, err := LoadKeysFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key, e, err := keys.Create("bob")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "pb-") || len(key) < 3+43 || e.Created.IsZero() {
		t.Fatalf("key %q entry %+v", key, e)
	}
	if name, err := authAs(t, keys, key); err != nil || name != "bob" {
		t.Fatalf("new key: %q %v", name, err)
	}
	if _, _, err := keys.Create("bob"); err != ErrKeyExists {
		t.Fatalf("duplicate: %v", err)
	}
	for _, bad := range []string{"", "a b", "anonymous", "admin", strings.Repeat("x", 65)} {
		if _, _, err := keys.Create(bad); err != ErrKeyName {
			t.Fatalf("name %q: %v", bad, err)
		}
	}

	st, _ := os.Stat(path)
	data, _ := os.ReadFile(path)
	if st.Mode().Perm() != 0o600 || strings.Contains(string(data), key) || strings.Contains(string(data), "sk-legacy") {
		t.Fatalf("file mode %v, content:\n%s", st.Mode(), data)
	}
	// The rewritten file (legacy entry now hashed) still authenticates both.
	again, err := LoadKeysFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{key: "bob", "sk-legacy": "legacy"} {
		if name, err := authAs(t, again, k); err != nil || name != want {
			t.Fatalf("reloaded %s: %q %v", want, name, err)
		}
	}

	if err := keys.Revoke("bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := authAs(t, keys, key); err != ErrUnauthorized {
		t.Fatalf("revoked key still works: %v", err)
	}
	if err := keys.Revoke("bob"); err != ErrKeyUnknown {
		t.Fatalf("revoke twice: %v", err)
	}
	// Reload picks up the revocation in the other store too.
	if err := again.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, err := authAs(t, again, key); err != ErrUnauthorized {
		t.Fatalf("reload kept revoked key: %v", err)
	}
	// A broken file is rejected and the current keys stay.
	os.WriteFile(path, []byte("broken\n"), 0o600)
	if err := again.Reload(); err == nil {
		t.Fatal("reload accepted a broken file")
	}
	if name, _ := authAs(t, again, "sk-legacy"); name != "legacy" {
		t.Fatal("failed reload dropped keys")
	}
}

func TestOpenKeysAttributeKnownKeysOnly(t *testing.T) {
	keys := NewStaticKeys("")
	if err := keys.Enforce(); err == nil {
		t.Fatal("enforced an empty store")
	}
	key, _, err := keys.Create("ci")
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{key: "ci", "": Anonymous, "whatever": Anonymous} {
		if name, err := authAs(t, keys, k); err != nil || name != want {
			t.Fatalf("open store, key %q: %q %v", k, name, err)
		}
	}
	if err := keys.Enforce(); err != nil || !keys.Enforced() {
		t.Fatal(err)
	}
	if _, err := authAs(t, keys, "whatever"); err != ErrUnauthorized {
		t.Fatalf("enforced store let an unknown key in: %v", err)
	}
	if name, _ := authAs(t, keys, key); name != "ci" {
		t.Fatal("enforced store rejects a valid key")
	}
}

type recorder struct {
	mu     sync.Mutex
	events []UsageEvent
}

func (r *recorder) Record(e UsageEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func TestUsageEvents(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	live := fakeLlama(t, "live")
	rec := &recorder{}
	g := New(AllowAll{}, &LeastInflight{}, func() []Backend {
		return []Backend{{NodeID: "dead", Model: "m", URL: dead.URL}, {NodeID: "live", Model: "m", URL: live.URL}}
	}, Config{UpstreamTimeout: 5 * time.Second, MaxAttempts: 2, Cooldown: time.Minute, Usage: rec},
		prometheus.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	g.Register(mux)

	post(mux, chat)
	post(mux, `{"model":"m","stream":true}`)
	post(mux, `{"model":"nope"}`)
	post(mux, `not json`)

	want := []UsageEvent{
		{Principal: Anonymous, NodeID: "dead", Model: "m", Code: CodeUpstreamError},
		{Principal: Anonymous, NodeID: "live", Model: "m", Code: "200", PromptTokens: 5, CompletionTokens: 7, GenTPS: 42},
		{Principal: Anonymous, NodeID: "live", Model: "m", Code: "200", PromptTokens: 5, CompletionTokens: 7, GenTPS: 42},
		{Principal: Anonymous, Model: "nope", Code: "404"},
		{Principal: Anonymous, Code: "400"},
	}
	// Order depends on the picker's rotation; compare as a multiset.
	seen := map[UsageEvent]int{}
	for _, e := range rec.events {
		seen[e]++
	}
	for _, e := range want {
		seen[e]--
	}
	for e, n := range seen {
		if n != 0 {
			t.Errorf("event %+v: count off by %d (all: %+v)", e, n, rec.events)
		}
	}
}

func TestDrainedNodeFinishesInflightButGetsNoNewRequests(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Write([]byte(`{"choices":[{"message":{"content":"hi from slow"}}]}`))
	}))
	defer slow.Close()
	live := fakeLlama(t, "live")

	var mu sync.Mutex
	drained := false
	g := New(AllowAll{}, &LeastInflight{}, func() []Backend {
		mu.Lock()
		defer mu.Unlock()
		out := []Backend{{NodeID: "slow", Model: "m", URL: slow.URL, Drained: drained}}
		if drained {
			out = append(out, Backend{NodeID: "live", Model: "m", URL: live.URL})
		}
		return out
	}, Config{UpstreamTimeout: time.Minute, MaxAttempts: 2, Cooldown: time.Minute},
		prometheus.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	g.Register(mux)

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- post(mux, chat) }()
	for g.Inflight()["slow"] == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	drained = true
	mu.Unlock()
	g.Reap() // must not cancel the drained node's request

	for i := 0; i < 3; i++ {
		if w := post(mux, chat); !strings.Contains(w.Body.String(), "hi from live") {
			t.Fatalf("new request went to drained node: %d %s", w.Code, w.Body)
		}
	}
	close(release)
	if w := <-done; w.Code != 200 || !strings.Contains(w.Body.String(), "hi from slow") {
		t.Fatalf("in-flight request on drained node: %d %s", w.Code, w.Body)
	}
}

func TestOnlyDrainedNodesMeansNoReadyNodes(t *testing.T) {
	_, h := newGW(t, AllowAll{}, Backend{NodeID: "a", Model: "m", URL: "http://x", Drained: true})
	if w := post(h, chat); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", w.Code)
	}
}

func TestSwitchPolicyAndTimeoutWhileServing(t *testing.T) {
	a, b := fakeLlama(t, "a"), fakeLlama(t, "b")
	g, h := newGW(t, AllowAll{}, Backend{NodeID: "a", Model: "m", URL: a.URL}, Backend{NodeID: "b", Model: "m", URL: b.URL})
	aff := NewAffinity(prometheus.NewRegistry())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if w := post(h, chat); w.Code != 200 {
					t.Errorf("status %d", w.Code)
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			g.SetPicker(aff)
		} else {
			g.SetPicker(&LeastInflight{})
		}
		g.SetUpstreamTimeout(time.Duration(i+1) * time.Second)
		aff.SetSpill(i%3 + 1)
	}
	wg.Wait()
	if g.UpstreamTimeout() != 20*time.Second || g.Picker() == Picker(aff) {
		t.Fatalf("timeout %v picker %T", g.UpstreamTimeout(), g.Picker())
	}
}

func TestUpstreamTimeoutAppliesToNewRequests(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	g, h := newGW(t, AllowAll{}, Backend{NodeID: "a", Model: "m", URL: slow.URL})
	g.SetUpstreamTimeout(50 * time.Millisecond)
	start := time.Now()
	if w := post(h, chat); w.Code != http.StatusBadGateway || time.Since(start) > time.Second {
		t.Fatalf("status %d after %v", w.Code, time.Since(start))
	}
}

func TestAffinityUnpinAndPins(t *testing.T) {
	p := NewAffinity(prometheus.NewRegistry())
	for _, k := range []string{"s1", "s2", "s3", "s4"} {
		p.Pick(Request{AffinityKey: k}, ab, idle)
	}
	pins := p.Pins()
	if pins["a"]+pins["b"] != 4 {
		t.Fatalf("pins = %v", pins)
	}
	if n := p.Unpin("a"); n != pins["a"] {
		t.Fatalf("unpinned %d, want %d", n, pins["a"])
	}
	if got := p.Pins(); got["a"] != 0 || got["b"] != pins["b"] {
		t.Fatalf("after unpin: %v", got)
	}
}
