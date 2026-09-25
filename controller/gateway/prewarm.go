package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// PrewarmTimeout bounds one node's prewarm request: a cold prompt of a few
// thousand tokens takes a phone minutes, anything longer is not worth
// waiting for.
const PrewarmTimeout = 120 * time.Second

// ErrNodeUnavailable is returned for a node target whose node is not ready.
var ErrNodeUnavailable = errors.New("node is not ready")

// PrewarmResult is one node's outcome of Prewarm.
type PrewarmResult struct {
	NodeID string `json:"node_id"`
	Alias  string `json:"alias"`
	OK     bool   `json:"ok"`
	Ms     int64  `json:"ms"`
	Error  string `json:"error"`
}

// prewarmTail is appended to the prewarm messages so the request is a valid
// chat turn; only the prefix before it matters for the cache.
var prewarmTail = json.RawMessage(`{"role":"user","content":"ok"}`)

// Prewarm sends every node of target, in parallel, a one-token chat
// completion with messages (and tools), so each node's llama-server caches
// that prompt prefix before real traffic arrives (ADR-014). target is
// resolved like a request's "model"; drained nodes are skipped.
func (g *Gateway) Prewarm(ctx context.Context, target string, messages []json.RawMessage, tools json.RawMessage) ([]PrewarmResult, error) {
	tgt, err := g.Resolve(target)
	if err != nil {
		return nil, err
	}
	nodes := tgt.filter(g.routable())
	if tgt.Node != "" && len(nodes) == 0 {
		return nil, ErrNodeUnavailable
	}
	msgs := append(append([]json.RawMessage{}, messages...), prewarmTail)
	out := make([]PrewarmResult, len(nodes))
	var wg sync.WaitGroup
	for i, b := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := map[string]any{"model": b.Model, "messages": msgs, "max_tokens": 1, "cache_prompt": true}
			if len(tools) > 0 && string(tools) != "null" {
				req["tools"] = tools
			}
			start := g.now()
			err := g.prewarmNode(ctx, b, req)
			out[i] = PrewarmResult{NodeID: b.NodeID, Alias: b.Alias, OK: err == nil, Ms: g.now().Sub(start).Milliseconds()}
			if err != nil {
				out[i].Error = err.Error()
			}
			g.log.Info("prewarm done", "node_id", b.NodeID, "target", target, "ok", err == nil, "duration_ms", out[i].Ms, "err", err)
		}()
	}
	wg.Wait()
	return out, nil
}

func (g *Gateway) prewarmNode(ctx context.Context, b Backend, req map[string]any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	g.addInflight(b.NodeID, 1) // pickers see the node busy meanwhile
	defer g.addInflight(b.NodeID, -1)
	ctx, cancel := context.WithTimeout(ctx, g.prewarmTimeout)
	defer cancel()
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, b.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	up.Header.Set("Content-Type", "application/json")
	setUpstreamAuth(up, b)
	resp, err := g.client.Do(up)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}
