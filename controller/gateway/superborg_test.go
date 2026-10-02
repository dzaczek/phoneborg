package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeOrchestrator mimics a streaming llama-server whose model calls
// delegate with calls (when the request offers tools), then answers with
// the tool results it was given. It records every request body.
type fakeOrchestrator struct {
	mu     sync.Mutex
	reqs   []map[string]any
	calls  string // delegate arguments; "" = answer directly
	always bool   // delegate whenever tools are offered, not only before any result
	fail   bool   // answer 500
}

func (f *fakeOrchestrator) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		f.mu.Unlock()
		if f.fail {
			http.Error(w, "loading model", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", b) }
		delta := func(d map[string]any) { send(map[string]any{"choices": []any{map[string]any{"delta": d}}}) }
		delta(map[string]any{"reasoning_content": "hmm"})
		var results []string
		for _, m := range req["messages"].([]any) {
			if m := m.(map[string]any); m["role"] == "tool" {
				results = append(results, m["content"].(string))
			}
		}
		if _, offered := req["tools"]; offered && f.calls != "" && (f.always || len(results) == 0) {
			// Split the arguments over two deltas, as llama-server streams them.
			half := len(f.calls) / 2
			delta(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function",
				"function": map[string]any{"name": "delegate", "arguments": f.calls[:half]}}}})
			delta(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": f.calls[half:]}}}})
		} else {
			delta(map[string]any{"content": "final: "})
			delta(map[string]any{"content": strings.Join(results, " | ")})
		}
		send(map[string]any{"choices": []any{}, "timings": map[string]any{"prompt_n": 10, "predicted_n": 3}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(s.Close)
	return s
}

func (f *fakeOrchestrator) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any{}, f.reqs...)
}

const twoTasks = `{"tasks":[{"worker":"w1","task":"say a"},{"worker":"w2","task":"say b"}]}`

func superborgGW(t *testing.T, orch *fakeOrchestrator, workers ...string) (*Gateway, http.Handler) {
	backends := []Backend{{NodeID: "o", Alias: "boss", Model: "big", URL: orch.server(t).URL}}
	for _, w := range workers {
		backends = append(backends, Backend{NodeID: w + "-id", Alias: w, Model: "small", URL: fakeLlama(t, w).URL, Speed: 20})
	}
	g, h := newGW(t, AllowAll{}, backends...)
	g.SetModelInfo(func(id string) (ModelInfo, bool) {
		return map[string]ModelInfo{"big": {SizeBytes: 5e9, Params: "8B"}, "small": {SizeBytes: 5e8, Params: "0.5B", Tags: []string{"fast"}}}[id], true
	})
	g.SetSuperborg(&Superborg{})
	return g, h
}

type completion struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

func TestSuperborgDelegatesInParallelAndAnswers(t *testing.T) {
	orch := &fakeOrchestrator{calls: twoTasks}
	g, h := superborgGW(t, orch, "w1", "w2")
	w := post(h, `{"model":"whatever","messages":[{"role":"system","content":"Be nice."},{"role":"user","content":"x"}]}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var c completion
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	msg := c.Choices[0].Message
	if msg.Content != "final: [w1] hi from w1\n\n[w2] hi from w2" {
		t.Errorf("content = %q", msg.Content)
	}
	for _, s := range []string{"hmm", "→ w1: say a", "→ w2: say b", "← w1: done", "← w2: done"} {
		if !strings.Contains(msg.ReasoningContent, s) {
			t.Errorf("reasoning %q lacks %q", msg.ReasoningContent, s)
		}
	}
	if c.Model != KindSuperborg || w.Header().Get("X-PhoneBorg-Node") != "o" {
		t.Errorf("model %q, node %q", c.Model, w.Header().Get("X-PhoneBorg-Node"))
	}
	if c.Usage.PromptTokens != 2*10+2*5 { // two orchestrator rounds, two worker calls
		t.Errorf("prompt tokens = %d", c.Usage.PromptTokens)
	}
	reqs := orch.requests()
	if len(reqs) != 2 {
		t.Fatalf("orchestrator got %d requests, want 2", len(reqs))
	}
	sys := reqs[0]["messages"].([]any)[0].(map[string]any)
	if s := sys["content"].(string); sys["role"] != "system" || !strings.Contains(s, "- w1: small (0.5B params; fast; ~20 tok/s)") ||
		!strings.HasSuffix(s, "\nBe nice.") {
		t.Errorf("system prompt = %v", sys)
	}
	if kw := reqs[0]["chat_template_kwargs"].(map[string]any); kw["enable_thinking"] != false {
		t.Errorf("thinking not off by default: %v", kw)
	}
	if reqs[0]["stream"] != true {
		t.Error("orchestrator request is not streamed")
	}
	for _, n := range []string{"w1-id", "w2-id"} {
		if got := testutil.ToFloat64(g.mDelegations.WithLabelValues(n, "ok")); got != 1 {
			t.Errorf("delegations to %s = %v", n, got)
		}
	}
	if got := testutil.ToFloat64(g.mSuperborg.WithLabelValues("ok")); got != 1 {
		t.Errorf("superborg ok = %v", got)
	}
}

func TestSuperborgStreams(t *testing.T) {
	orch := &fakeOrchestrator{calls: twoTasks}
	_, h := superborgGW(t, orch, "w1", "w2")
	w := post(h, `{"model":"x","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	body := w.Body.String()
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, type %q: %s", w.Code, w.Header().Get("Content-Type"), body)
	}
	for _, s := range []string{`"reasoning_content":"\n→ w1: say a\n"`, `"content":"final: "`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, s) {
			t.Errorf("stream lacks %s:\n%s", s, body)
		}
	}
}

func TestSuperborgSimpleQuestionNoDelegation(t *testing.T) {
	orch := &fakeOrchestrator{}
	_, h := superborgGW(t, orch, "w1")
	w := post(h, chat)
	var c completion
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if w.Code != 200 || c.Choices[0].Message.Content != "final: " || len(orch.requests()) != 1 {
		t.Fatalf("status %d, %d orchestrator requests: %s", w.Code, len(orch.requests()), w.Body)
	}
}

func TestSuperborgLastRoundHasNoTools(t *testing.T) {
	orch := &fakeOrchestrator{calls: `{"worker":"w1","task":"again"}`, always: true} // bare task, delegates every time it may
	_, h := superborgGW(t, orch, "w1")
	if w := post(h, chat); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	reqs := orch.requests()
	if len(reqs) != superborgMaxRounds+1 {
		t.Fatalf("orchestrator got %d requests, want %d", len(reqs), superborgMaxRounds+1)
	}
	if _, ok := reqs[len(reqs)-1]["tools"]; ok {
		t.Error("final round still offers tools")
	}
}

func TestSuperborgClientToolsGoStraightToOrchestrator(t *testing.T) {
	orch := &fakeOrchestrator{calls: twoTasks}
	_, h := superborgGW(t, orch, "w1")
	w := post(h, `{"model":"x","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"read"}}]}`)
	if w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "o" {
		t.Fatalf("status %d node %q: %s", w.Code, w.Header().Get("X-PhoneBorg-Node"), w.Body)
	}
	reqs := orch.requests()
	if len(reqs) != 1 || strings.Contains(fmt.Sprint(reqs[0]["messages"]), "SuperBorg") {
		t.Errorf("request was rewritten: %v", reqs)
	}
}

func TestSuperborgWorkerFailureRetriesElsewhere(t *testing.T) {
	orch := &fakeOrchestrator{calls: `{"tasks":[{"worker":"bad","task":"t"}]}`}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }))
	t.Cleanup(broken.Close)
	g, h := newGW(t, AllowAll{},
		Backend{NodeID: "o", Model: "big", URL: orch.server(t).URL, Speed: 9},
		Backend{NodeID: "bad", Model: "small", URL: broken.URL},
		Backend{NodeID: "good", Model: "small", URL: fakeLlama(t, "good").URL})
	g.SetSuperborg(&Superborg{Orchestrator: "o"})
	w := post(h, chat)
	var c completion
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if w.Code != 200 || c.Choices[0].Message.Content != "final: [good] hi from good" {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

func TestSuperborgOrchestratorFailover(t *testing.T) {
	dead := &fakeOrchestrator{fail: true}
	alive := &fakeOrchestrator{}
	g, h := newGW(t, AllowAll{},
		Backend{NodeID: "dead", Model: "m", URL: dead.server(t).URL},
		Backend{NodeID: "alive", Model: "m", URL: alive.server(t).URL})
	g.SetSuperborg(&Superborg{Orchestrator: "dead"})
	w := post(h, chat)
	if w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "alive" || len(alive.requests()) != 1 {
		t.Fatalf("status %d node %q: %s", w.Code, w.Header().Get("X-PhoneBorg-Node"), w.Body)
	}
}

func TestSuperborgPickOrchestrator(t *testing.T) {
	g, _ := newGW(t, AllowAll{},
		Backend{NodeID: "a", Model: "small", Speed: 30},
		Backend{NodeID: "b", Alias: "oneplus", Model: "big", Speed: 3},
		Backend{NodeID: "c", Model: "mid", Speed: 7})
	g.SetModelInfo(func(id string) (ModelInfo, bool) {
		return ModelInfo{SizeBytes: map[string]int64{"small": 1, "mid": 5, "big": 9}[id]}, true
	})
	for cfg, want := range map[string]string{"": "b", "oneplus": "b", "c": "c", "gone": "b"} {
		if o, ok := g.pickOrchestrator(Superborg{Orchestrator: cfg}, nil); !ok || o.NodeID != want {
			t.Errorf("orchestrator %q: got %s, want %s", cfg, o.NodeID, want)
		}
	}
	o, workers, _ := g.SuperborgPlan(Superborg{})
	if o.NodeID != "b" || len(workers) != 2 || workers[0].NodeID != "a" {
		t.Errorf("plan: %s %v", o.NodeID, workers)
	}
}

func TestSuperborgModelList(t *testing.T) {
	orch := &fakeOrchestrator{}
	g, _ := superborgGW(t, orch, "w1")
	e := g.ModelEntries()
	if len(e) != 1 || e[0].ID != KindSuperborg || e[0].Nodes != 2 || e[0].Description != "orchestrator boss (big)" {
		t.Errorf("entries = %+v", e)
	}
	g.SetSuperborg(nil)
	if e := g.ModelEntries(); len(e) < 3 {
		t.Errorf("normal mode entries = %+v", e)
	}
}

func TestParseSubtasks(t *testing.T) {
	for args, want := range map[string]int{twoTasks: 2, `{"worker":"a","task":"t"}`: 1, `{"tasks":[]}`: 0, `nope`: 0} {
		got, err := parseSubtasks(args)
		if len(got) != want || (want == 0) != (err != nil) {
			t.Errorf("%s: %v, %v", args, got, err)
		}
	}
}
