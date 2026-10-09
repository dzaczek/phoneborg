package gateway

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// routerTargets: node/cls is the classifier, node/small and node/big the
// easy and hard targets, pool/off a disabled pool.
type routerTargets struct{}

func (routerTargets) Resolve(name string) (Target, bool) {
	switch name {
	case "node/cls":
		return Target{Label: name, Node: "cls"}, true
	case "node/small":
		return Target{Label: name, Node: "small"}, true
	case "node/big":
		return Target{Label: name, Node: "big"}, true
	case "pool/off":
		return Target{Label: name, Nodes: map[string]bool{"big": true}, Disabled: true}, true
	}
	return Target{}, false
}

func (routerTargets) Models() []ModelEntry { return nil }

// fakeClassifier answers like llama-server with logprobs: first is the
// sampled token, tops its top_logprobs as token → probability. calls counts
// requests; each must ask for one token with logprobs.
func fakeClassifier(t *testing.T, status int, first string, tops map[string]float64, calls *atomic.Int32) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			MaxTokens   int  `json:"max_tokens"`
			Logprobs    bool `json:"logprobs"`
			TopLogprobs int  `json:"top_logprobs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.MaxTokens != 1 || !req.Logprobs || req.TopLogprobs == 0 {
			t.Errorf("classifier request = %+v", req)
		}
		if status != http.StatusOK {
			http.Error(w, "boom", status)
			return
		}
		var lp []string
		for tok, p := range tops {
			lp = append(lp, fmt.Sprintf(`{"token":%q,"logprob":%v}`, tok, math.Log(p)))
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"logprobs":{"content":[{"token":%q,"logprob":-0.1,"top_logprobs":[%s]}]}}]}`,
			first, first, strings.Join(lp, ","))
	}))
	t.Cleanup(s.Close)
	return s
}

// routerGW is a gateway with nodes cls (classifier), small and big, all
// serving model m, the router on: easy → node/small, hard → node/big.
func routerGW(t *testing.T, cls *httptest.Server) (*Gateway, http.Handler) {
	small, big := fakeLlama(t, "small"), fakeLlama(t, "big")
	g, h := newGW(t, AllowAll{},
		Backend{NodeID: "cls", Model: "m", URL: cls.URL},
		Backend{NodeID: "small", Model: "m", URL: small.URL},
		Backend{NodeID: "big", Model: "m", URL: big.URL})
	g.SetTargets(routerTargets{})
	g.SetRouter(RouterConfig{Enabled: true, Classifier: "node/cls", Easy: "node/small", Hard: "node/big",
		Threshold: 0.5, Timeout: 5 * time.Second})
	return g, h
}

const autoChat = `{"model":"auto","messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi there"}]}`

func TestRouterSendsEasyAndHard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first string
		tops  map[string]float64
		route string
		node  string
	}{
		{"easy", "A", map[string]float64{"A": 0.9, "B": 0.08}, RouteEasy, "small"},
		{"hard", " B", map[string]float64{" B": 0.7, "A": 0.2, "```": 0.05}, RouteHard, "big"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			g, h := routerGW(t, fakeClassifier(t, 200, tc.first, tc.tops, &calls))
			w := post(h, autoChat)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "hi from "+tc.node) {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if got := w.Header().Get("X-Phoneborg-Route"); got != tc.route {
				t.Errorf("route header = %q", got)
			}
			if calls.Load() != 1 {
				t.Errorf("classifier calls = %d", calls.Load())
			}
			if got := testutil.ToFloat64(g.mRouter.WithLabelValues(tc.route)); got != 1 {
				t.Errorf("decisions{%s} = %v", tc.route, got)
			}
		})
	}
}

// Every classifier failure serves the request as plain "auto".
func TestRouterFallsBackToAuto(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		first  string
		tops   map[string]float64
		cfg    func(*RouterConfig)
	}{
		{"server error", 500, "", nil, nil},
		{"not a letter", 200, "```", map[string]float64{"```": 0.9}, nil},
		{"little mass on A and B", 200, "Here", map[string]float64{"Here": 0.6, "A": 0.1, "B": 0.05}, nil},
		{"unknown classifier", 200, "A", map[string]float64{"A": 1}, func(c *RouterConfig) { c.Classifier = "node/nope" }},
		{"unknown target", 200, "A", map[string]float64{"A": 1}, func(c *RouterConfig) { c.Easy = "pool/nope" }},
		{"disabled target", 200, "B", map[string]float64{"B": 1}, func(c *RouterConfig) { c.Hard = "pool/off" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			g, h := routerGW(t, fakeClassifier(t, tc.status, tc.first, tc.tops, &calls))
			if tc.cfg != nil {
				c := g.Router()
				tc.cfg(&c)
				g.SetRouter(c)
			}
			w := post(h, autoChat)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "hi from") {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if got := w.Header().Get("X-Phoneborg-Route"); got != RouteFallback {
				t.Errorf("route header = %q", got)
			}
			if got := testutil.ToFloat64(g.mRouter.WithLabelValues(RouteFallback)); got != 1 {
				t.Errorf("fallbacks = %v", got)
			}
		})
	}
}

func TestRouterOnlyForEnabledAuto(t *testing.T) {
	var calls atomic.Int32
	g, h := routerGW(t, fakeClassifier(t, 200, "B", map[string]float64{"B": 1}, &calls))
	if w := post(h, strings.Replace(autoChat, `"auto"`, `"m"`, 1)); w.Code != 200 || w.Header().Get("X-Phoneborg-Route") != "" {
		t.Fatalf("model request: %d route %q", w.Code, w.Header().Get("X-Phoneborg-Route"))
	}
	c := g.Router()
	c.Enabled = false
	g.SetRouter(c)
	if w := post(h, autoChat); w.Code != 200 || w.Header().Get("X-Phoneborg-Route") != "" {
		t.Fatalf("router off: %d route %q", w.Code, w.Header().Get("X-Phoneborg-Route"))
	}
	if calls.Load() != 0 {
		t.Fatalf("classifier called %d times", calls.Load())
	}
}

func TestRequestText(t *testing.T) {
	meta := func(body string) requestMeta {
		var m requestMeta
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for body, want := range map[string]string{
		autoChat: "hi there",
		`{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"x"},{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"u"}}]}]}`: "look",
		`{"prompt":"complete me"}`:                                 "complete me",
		`{"messages":[{"role":"system","content":"only system"}]}`: "",
	} {
		if got := requestText(meta(body)); got != want {
			t.Errorf("%s: got %q, want %q", body, got, want)
		}
	}
	long := requestText(meta(`{"prompt":"` + strings.Repeat("x", 3*routerMaxText) + `"}`))
	if len(long) > routerMaxText+len("…") {
		t.Errorf("long text not clipped: %d bytes", len(long))
	}
}
