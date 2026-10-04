package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ProbeResult is one timed request of a benchmark (ADR-028).
type ProbeResult struct {
	TTFTMs           float64 `json:"ttft_ms"`  // until the first streamed token (content or reasoning)
	TotalMs          float64 `json:"total_ms"` // until the stream ended
	PromptTokens     int     `json:"prompt_tokens"`
	CachedTokens     int     `json:"cached_tokens"` // prompt tokens reused from the node's cache
	CompletionTokens int     `json:"completion_tokens"`
	PromptTPS        float64 `json:"prompt_tps"` // llama.cpp's prompt processing speed
	GenTPS           float64 `json:"gen_tps"`    // llama.cpp's generation speed
	Text             string  `json:"text,omitempty"`
}

// ErrProbeNode is returned when the node is not a ready backend.
var ErrProbeNode = errors.New("node is not ready")

// firstToken records when the first text of a stream arrived.
type firstToken struct {
	mu    sync.Mutex
	start time.Time
	at    time.Duration
	now   func() time.Time
}

func (f *firstToken) send(_, text string) {
	if text == "" {
		return
	}
	f.mu.Lock()
	if f.at == 0 {
		f.at = f.now().Sub(f.start)
	}
	f.mu.Unlock()
}

// Probe sends one streamed chat request straight to nodeID, also when it is
// drained (a benchmark drains the node it measures), and times it. body is
// an OpenAI chat request; "stream" is forced on.
func (g *Gateway) Probe(ctx context.Context, nodeID string, body map[string]any, p Principal) (ProbeResult, error) {
	var b Backend
	found := false
	for _, x := range g.backends() {
		if x.NodeID == nodeID {
			b, found = x, true
		}
	}
	if !found {
		return ProbeResult{}, ErrProbeNode
	}
	body["stream"] = true
	body["model"] = b.Model
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))
	if err != nil {
		return ProbeResult{}, err
	}
	r.Header.Set("X-Request-Id", "mmb-"+newID())
	ft := &firstToken{start: g.now(), now: g.now}
	sw := &sseCollector{header: http.Header{}, em: ft}
	err = g.forward(sw, r, b, mustJSON(body), true, p, r.Header.Get("X-Request-Id"))
	total := g.now().Sub(ft.start)
	if err != nil {
		return ProbeResult{}, err
	}
	if sw.status != http.StatusOK {
		return ProbeResult{}, fmt.Errorf("HTTP %d: %s", sw.status, clip(extractOpenAIErrorMessage(sw.errBody.Bytes()), 200))
	}
	if !sw.sawEvent {
		sw.whole(bytes.TrimSpace(sw.line))
	}
	ft.mu.Lock()
	ttft := ft.at
	ft.mu.Unlock()
	return ProbeResult{
		TTFTMs: float64(ttft.Microseconds()) / 1000, TotalMs: float64(total.Microseconds()) / 1000,
		PromptTokens: sw.usage.PromptTokens, CachedTokens: sw.usage.CachedTokens, CompletionTokens: sw.usage.CompletionTokens,
		PromptTPS: sw.usage.PromptTPS, GenTPS: sw.usage.GenTPS, Text: clip(stripThink(sw.content.String()), 300),
	}, nil
}
