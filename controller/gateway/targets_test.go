package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeTargets knows pool "fast" (nodes a and b, spread), pool "empty" (no
// eligible node) and node "phone-01" (alias of a). Like the controller it
// shares one Spread picker across requests.
type fakeTargets struct{ spread *Spread }

func (f fakeTargets) Resolve(name string) (Target, bool) {
	switch name {
	case "pool/fast":
		return Target{Label: name, Nodes: map[string]bool{"a": true, "b": true}, Picker: f.spread}, true
	case "pool/empty":
		return Target{Label: name, Nodes: map[string]bool{}}, true
	case "node/phone-01", "node/a":
		return Target{Label: name, Node: "a"}, true
	case "node/c":
		return Target{Label: name, Node: "c"}, true
	}
	return Target{}, false
}

func (fakeTargets) Models() []ModelEntry {
	ready := true
	return []ModelEntry{
		{ID: "pool/fast", Object: "model", OwnedBy: "phoneborg", Kind: KindPool, Nodes: 2, Description: "small agents"},
		{ID: "node/phone-01", Object: "model", OwnedBy: "phoneborg", Kind: KindNode, Nodes: 1, Model: "m", Ready: &ready},
	}
}

func TestResolve(t *testing.T) {
	g, _ := newGW(t, AllowAll{})
	for _, tc := range []struct {
		name, label, model, node string
		err                      error
	}{
		{name: "auto", label: "auto"},
		{name: "qwen", label: "model", model: "qwen"},
		{name: "", label: "model"},
		{name: "pool/fast", err: ErrUnknownTarget}, // no Targets installed yet
		{name: "node/a", err: ErrUnknownTarget},
	} {
		got, err := g.Resolve(tc.name)
		if !errors.Is(err, tc.err) || got.Label != tc.label || got.Model != tc.model || got.Node != tc.node {
			t.Errorf("%q: got %+v, %v", tc.name, got, err)
		}
	}
	g.SetTargets(fakeTargets{&Spread{}})
	for _, tc := range []struct {
		name, label, node string
		err               error
	}{
		{name: "pool/fast", label: "pool/fast"},
		{name: "node/phone-01", label: "node/phone-01", node: "a"},
		{name: "pool/nope", err: ErrUnknownTarget},
		{name: "node/nope", err: ErrUnknownTarget},
		{name: "auto", label: "auto"},
	} {
		got, err := g.Resolve(tc.name)
		if !errors.Is(err, tc.err) || got.Label != tc.label || got.Node != tc.node {
			t.Errorf("%q: got %+v, %v", tc.name, got, err)
		}
	}
}

func TestSpreadPicker(t *testing.T) {
	busy := func(n map[string]int) func(string) int { return func(id string) int { return n[id] } }
	for _, tc := range []struct {
		name     string
		cands    []Backend
		inflight map[string]int
		want     string
	}{
		{"least in-flight wins over speed", []Backend{{NodeID: "a", Speed: 30}, {NodeID: "b", Speed: 5}}, map[string]int{"a": 1}, "b"},
		{"then the fastest", []Backend{{NodeID: "a", Speed: 5}, {NodeID: "b", Speed: 30}}, nil, "b"},
		{"hot avoided", []Backend{{NodeID: "a", Speed: 30, Hot: true}, {NodeID: "b", Speed: 5}}, nil, "b"},
		{"all hot still served", []Backend{{NodeID: "a", Hot: true}}, nil, "a"},
	} {
		for i := 0; i < 4; i++ { // regardless of rotation
			p := &Spread{}
			p.rr.Store(uint64(i))
			if got := p.Pick(Request{AffinityKey: "k"}, tc.cands, busy(tc.inflight)); got.NodeID != tc.want {
				t.Errorf("%s: got %s", tc.name, got.NodeID)
			}
		}
	}
	// Full ties rotate.
	p, seen := &Spread{}, map[string]int{}
	for i := 0; i < 6; i++ {
		seen[p.Pick(Request{AffinityKey: "same"}, ab, idle).NodeID]++
	}
	if seen["a"] != 3 || seen["b"] != 3 {
		t.Fatalf("ties did not rotate: %v", seen)
	}
}

// newTargetGW is a gateway with affinity as its policy and fakeTargets.
func newTargetGW(t *testing.T, backends ...Backend) (*Gateway, *Affinity, http.Handler) {
	aff := NewAffinity(prometheus.NewRegistry())
	g, h := newGW(t, AllowAll{}, backends...)
	g.SetPicker(aff)
	g.SetTargets(fakeTargets{&Spread{}})
	return g, aff, h
}

func TestPoolTargetSpreadsWithoutPinning(t *testing.T) {
	a, b, c := fakeLlama(t, "a"), fakeLlama(t, "b"), fakeLlama(t, "c")
	g, aff, h := newTargetGW(t, Backend{NodeID: "a", Model: "m", URL: a.URL}, Backend{NodeID: "b", Model: "m2", URL: b.URL},
		Backend{NodeID: "c", Model: "m", URL: c.URL})
	body := `{"model":"pool/fast","messages":[{"role":"system","content":"same prompt"}]}`
	for i := 0; i < 6; i++ {
		w := post(h, body)
		if w.Code != 200 || strings.Contains(w.Body.String(), "hi from c") {
			t.Fatalf("status %d body %s", w.Code, w.Body)
		}
	}
	for _, n := range []struct{ node, model string }{{"a", "m"}, {"b", "m2"}} {
		if got := testutil.ToFloat64(g.mRequests.WithLabelValues(n.model, n.node, "200", "anonymous")); got != 3 {
			t.Errorf("node %s served %v, want 3 (spread, not pinned)", n.node, got)
		}
	}
	if len(aff.Pins()) != 0 {
		t.Fatalf("spread pool touched affinity pins: %v", aff.Pins())
	}
	if got := testutil.ToFloat64(g.mTargets.WithLabelValues("pool/fast")); got != 6 {
		t.Errorf("target metric = %v", got)
	}
	if w := post(h, `{"model":"pool/empty"}`); w.Code != 503 {
		t.Fatalf("empty pool: %d %s", w.Code, w.Body)
	}
	if w := post(h, `{"model":"pool/nope"}`); w.Code != 404 || !strings.Contains(w.Body.String(), "model_not_found") {
		t.Fatalf("unknown pool: %d %s", w.Code, w.Body)
	}
}

func TestAutoTargetUsesAnyModelAndAffinity(t *testing.T) {
	a, b := fakeLlama(t, "a"), fakeLlama(t, "b")
	g, aff, h := newTargetGW(t, Backend{NodeID: "a", Model: "m1", URL: a.URL}, Backend{NodeID: "b", Model: "m2", URL: b.URL})
	for i := 0; i < 3; i++ {
		if w := post(h, `{"model":"auto","messages":[{"role":"system","content":"S"}]}`); w.Code != 200 {
			t.Fatalf("auto: %d %s", w.Code, w.Body)
		}
	}
	if len(aff.Pins()) != 1 {
		t.Fatalf("auto should pin its session: %v", aff.Pins())
	}
	if got := testutil.ToFloat64(g.mTargets.WithLabelValues("auto")); got != 3 {
		t.Errorf("target metric = %v", got)
	}
}

func TestNodeTarget(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	b := fakeLlama(t, "b")
	for _, tc := range []struct {
		name     string
		backends []Backend
		body     string
		code     int
		want     string
	}{
		{"alias routes to its node even when busier", []Backend{{NodeID: "a", Model: "m", URL: fakeLlama(t, "a").URL, Hot: true}, {NodeID: "b", Model: "m", URL: b.URL, Speed: 99}},
			`{"model":"node/phone-01"}`, 200, "hi from a"},
		{"no failover when the node fails", []Backend{{NodeID: "a", Model: "m", URL: dead.URL}, {NodeID: "b", Model: "m", URL: b.URL}},
			`{"model":"node/a"}`, 502, "backends_failed"},
		{"not ready", []Backend{{NodeID: "b", Model: "m", URL: b.URL}}, `{"model":"node/phone-01"}`, 503, "node_unavailable"},
		{"drained", []Backend{{NodeID: "a", Model: "m", URL: b.URL, Drained: true}}, `{"model":"node/a"}`, 503, "node_unavailable"},
		{"context too small", []Backend{{NodeID: "a", Model: "m", URL: b.URL, CtxSize: 100}, {NodeID: "b", Model: "m", URL: b.URL}},
			`{"model":"node/a","messages":[{"role":"user","content":"` + strings.Repeat("x", 2000) + `"}]}`, 400, "context_length_exceeded"},
		{"unknown node", []Backend{{NodeID: "a", Model: "m", URL: b.URL}}, `{"model":"node/zz"}`, 404, "model_not_found"},
	} {
		g, _, h := newTargetGW(t, tc.backends...)
		w := post(h, tc.body)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		if got := testutil.ToFloat64(g.mRequests.WithLabelValues("m", "b", "200", "anonymous")); got != 0 {
			t.Errorf("%s: request failed over to b", tc.name)
		}
	}
}

func TestModelsListKinds(t *testing.T) {
	_, _, h := newTargetGW(t, Backend{NodeID: "a", Model: "m1", URL: "x"}, Backend{NodeID: "b", Model: "m1", URL: "y"},
		Backend{NodeID: "c", Model: "m2", URL: "z"}, Backend{NodeID: "d", Model: "m2", URL: "z", Drained: true})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var out struct{ Data []ModelEntry }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	want := []string{"m1 model 2", "m2 model 1", "auto auto 3", "pool/fast pool 2", "node/phone-01 node 1"}
	if len(out.Data) != len(want) {
		t.Fatalf("models: %s", w.Body)
	}
	for i, e := range out.Data {
		if got := e.ID + " " + e.Kind + " " + strconv.Itoa(e.Nodes); got != want[i] || e.Object != "model" || e.OwnedBy != "phoneborg" {
			t.Errorf("entry %d = %+v, want %s", i, e, want[i])
		}
	}
	if !strings.Contains(w.Body.String(), `"description":"small agents"`) || !strings.Contains(w.Body.String(), `"model":"m","ready":true`) {
		t.Errorf("extra fields missing: %s", w.Body)
	}
}

func TestPrewarm(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string]map[string]any{}
	record := func(name string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var v map[string]any
			_ = json.NewDecoder(r.Body).Decode(&v)
			mu.Lock()
			bodies[name] = v
			mu.Unlock()
			w.Write([]byte(`{"choices":[]}`))
		}))
		t.Cleanup(s.Close)
		return s
	}
	stop := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer hang.Close()
	defer close(stop)

	g, _, _ := newTargetGW(t, Backend{NodeID: "a", Model: "m", URL: record("a").URL, Alias: "phone-01"},
		Backend{NodeID: "b", Model: "m", URL: hang.URL}, Backend{NodeID: "c", Model: "m", URL: record("c").URL})
	g.prewarmTimeout = 200 * time.Millisecond
	msgs := []json.RawMessage{json.RawMessage(`{"role":"system","content":"You are terse."}`)}

	res, err := g.Prewarm(t.Context(), "pool/fast", msgs, json.RawMessage(`[{"type":"function"}]`))
	if err != nil || len(res) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if !res[0].OK || res[0].NodeID != "a" || res[0].Alias != "phone-01" || res[1].OK || res[1].NodeID != "b" || res[1].Error == "" {
		t.Fatalf("results %+v", res)
	}
	body := bodies["a"]
	m, _ := json.Marshal(body["messages"])
	if string(m) != `[{"content":"You are terse.","role":"system"},{"content":"ok","role":"user"}]` ||
		body["max_tokens"] != float64(1) || body["cache_prompt"] != true || body["tools"] == nil {
		t.Fatalf("prewarm body %v", body)
	}
	if _, seen := bodies["c"]; seen {
		t.Fatal("node outside the pool was prewarmed")
	}
	if g.inflightOf("a") != 0 || g.inflightOf("b") != 0 {
		t.Fatal("in-flight count leaked")
	}

	if res, err := g.Prewarm(t.Context(), "node/phone-01", msgs, nil); err != nil || len(res) != 1 || !res[0].OK {
		t.Fatalf("node: %+v %v", res, err)
	}
	if _, err := g.Prewarm(t.Context(), "node/c", msgs, nil); err != nil {
		t.Fatalf("node c: %v", err)
	}
	if _, err := g.Prewarm(t.Context(), "pool/nope", msgs, nil); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("unknown pool: %v", err)
	}
	g2, _, _ := newTargetGW(t, Backend{NodeID: "b", Model: "m", URL: hang.URL})
	if _, err := g2.Prewarm(t.Context(), "node/phone-01", msgs, nil); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("unavailable node: %v", err)
	}
}
