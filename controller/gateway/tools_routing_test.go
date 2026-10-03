package gateway

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

const toolsChat = `{"model":"auto","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"read"}}]}`

func TestToolRequestsGoToToolCapableModels(t *testing.T) {
	g, h := newGW(t, AllowAll{},
		Backend{NodeID: "gemma", Model: "gemma", URL: fakeLlama(t, "gemma").URL},
		Backend{NodeID: "qwen", Model: "qwen", URL: fakeLlama(t, "qwen").URL})
	g.SetModelInfo(func(id string) (ModelInfo, bool) {
		return map[string]ModelInfo{"qwen": {Tags: []string{"general", ToolsTag}}}[id], true
	})
	for i := 0; i < 4; i++ {
		if w := post(h, toolsChat); w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "qwen" {
			t.Fatalf("tools request went to %q: %d", w.Header().Get("X-PhoneBorg-Node"), w.Code)
		}
	}
	// Without tools both nodes serve.
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		seen[post(h, `{"model":"auto","messages":[{"role":"user","content":"x"}],"tools":[]}`).Header().Get("X-PhoneBorg-Node")] = true
	}
	if !seen["gemma"] || !seen["qwen"] {
		t.Errorf("plain requests served by %v", seen)
	}
	if got := testutil.ToFloat64(g.mToolRouting.WithLabelValues("capable")); got != 4 {
		t.Errorf("capable = %v", got)
	}
}

func TestToolRequestsFallBackWithoutCapableModels(t *testing.T) {
	g, h := newGW(t, AllowAll{}, Backend{NodeID: "gemma", Model: "gemma", URL: fakeLlama(t, "gemma").URL})
	if w := post(h, toolsChat); w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "gemma" {
		t.Fatalf("status %d node %q", w.Code, w.Header().Get("X-PhoneBorg-Node"))
	}
	if got := testutil.ToFloat64(g.mToolRouting.WithLabelValues("fallback")); got != 1 {
		t.Errorf("fallback = %v", got)
	}
}

func TestOrchestratorPrefersToolCapableModel(t *testing.T) {
	g, _ := newGW(t, AllowAll{},
		Backend{NodeID: "mi8", Model: "gemma-3n", Speed: 4},
		Backend{NodeID: "poco", Model: "qwen3-4b", Speed: 7})
	g.SetModelInfo(func(id string) (ModelInfo, bool) {
		return map[string]ModelInfo{
			"gemma-3n": {SizeBytes: 2800},                           // the larger file
			"qwen3-4b": {SizeBytes: 2300, Tags: []string{ToolsTag}}, // but this one calls tools
		}[id], true
	})
	if o, _ := g.pickOrchestrator(Superborg{}, g.routable(), nil); o.NodeID != "poco" {
		t.Errorf("orchestrator %s", o.NodeID)
	}
}
