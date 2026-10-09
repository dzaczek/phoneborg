package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// Semantic router (ADR-033, experimental, off by default). When enabled, a
// request for "auto" is first classified as easy or hard by one phone, and
// then served by the Easy or Hard target. The classifier generates a single
// token: the gateway reads the probabilities of the two answer letters from
// llama-server's top_logprobs, so the cost is prompt processing only.
// Any failure (no classifier node, timeout, no usable logprobs, an unknown
// target) falls back to plain "auto": the router can only make routing
// smarter, never make a request fail.

// RouterConfig is the semantic router's runtime configuration.
type RouterConfig struct {
	Enabled bool
	// Classifier is the routing target that classifies, e.g. "node/mi8";
	// Easy and Hard are the targets each class is sent to ("auto",
	// "pool/<name>", "node/<alias>" or a model id; "" = "auto").
	Classifier, Easy, Hard string
	// Threshold: P(hard) at or above it sends the request to Hard.
	Threshold float64
	// Timeout bounds the classification; on expiry the request is plain "auto".
	Timeout time.Duration
}

// DefaultRouterConfig is the router's configuration until the operator
// changes it: disabled.
var DefaultRouterConfig = RouterConfig{Threshold: 0.5, Timeout: 20 * time.Second}

// Router decision results: the metric label and the X-Phoneborg-Route header.
const (
	RouteEasy     = "easy"
	RouteHard     = "hard"
	RouteFallback = "fallback"
)

// routerMaxText caps the request text shown to the classifier: prompt
// processing is what a decision costs, and the start of a request says
// enough about its difficulty.
const routerMaxText = 2000

// routerMinMass is the least probability the classifier must put on the
// two answer letters together. Below it the model did not answer the
// question (a small model may start doing the request instead), and its
// A/B split means nothing.
const routerMinMass = 0.5

// routerSystem is constant, so the classifier node caches it and only the
// request text is processed per decision.
const routerSystem = `You are a request classifier. You never answer or carry out the request; you only say how hard it is for a small language model.
A = EASY: greeting or chit-chat, a short factual question, a simple rewrite, a translation or a short summary.
B = HARD: multi-step reasoning, math or proofs, writing or debugging code, long or technical writing, analysis or planning.

<request>Hello! How are you today?</request> Answer: A
<request>Translate "thank you very much" into Spanish.</request> Answer: A
<request>Who wrote "Pride and Prejudice"?</request> Answer: A
<request>Implement an LRU cache in Python with O(1) operations.</request> Answer: B
<request>Prove that the square root of 2 is irrational.</request> Answer: B
<request>Design a backup strategy for a 3-node Postgres cluster.</request> Answer: B

Reply with one letter only: A or B.`

// SetRouter installs the semantic router configuration.
func (g *Gateway) SetRouter(c RouterConfig) { g.router.Store(&c) }

// Router returns the semantic router configuration.
func (g *Gateway) Router() RouterConfig { return *g.router.Load() }

// routeAuto classifies an "auto" request and returns the target it should
// go to; on any failure it returns tgt unchanged.
func (g *Gateway) routeAuto(w http.ResponseWriter, r *http.Request, tgt Target, meta requestMeta, reqID string) Target {
	cfg := g.Router()
	if !cfg.Enabled {
		return tgt
	}
	log := g.log.With("request_id", reqID)
	start := g.now()
	result, pHard, next, err := g.classify(r.Context(), cfg, meta)
	g.mRouterSeconds.Observe(g.now().Sub(start).Seconds())
	if err != nil {
		result, next = RouteFallback, tgt
		log.Warn("router fallback to auto", "err", err)
	} else {
		log.Info("router decision", "route", result, "p_hard", math.Round(pHard*1000)/1000, "target", next.Label,
			"duration_ms", g.now().Sub(start).Milliseconds())
	}
	g.mRouter.WithLabelValues(result).Inc()
	w.Header().Set("X-Phoneborg-Route", result)
	return next
}

// classify asks the classifier for P(hard) and resolves the chosen target.
func (g *Gateway) classify(ctx context.Context, cfg RouterConfig, meta requestMeta) (string, float64, Target, error) {
	text := requestText(meta)
	if text == "" {
		return "", 0, Target{}, errors.New("no user text to classify")
	}
	ct, err := g.Resolve(cfg.Classifier)
	if err != nil {
		return "", 0, Target{}, fmt.Errorf("classifier %q: %w", cfg.Classifier, err)
	}
	nodes := g.candidates(ct.filter(g.routable()), nil, 0)
	if len(nodes) == 0 {
		return "", 0, Target{}, fmt.Errorf("classifier %q has no ready node", cfg.Classifier)
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	b := g.leastBusy(nodes, nil)
	pHard, mass, err := g.askClassifier(ctx, b, text)
	if err != nil {
		return "", 0, Target{}, err
	}
	if mass < routerMinMass {
		return "", 0, Target{}, fmt.Errorf("classifier %s put only %.2f on the answers A and B (model unsuitable as a classifier?)", b.NodeID, mass)
	}
	result, name := RouteEasy, cfg.Easy
	if pHard >= cfg.Threshold {
		result, name = RouteHard, cfg.Hard
	}
	if name == "" {
		name = KindAuto
	}
	next, err := g.Resolve(name)
	if err != nil {
		return "", 0, Target{}, fmt.Errorf("%s target %q: %w", result, name, err)
	}
	if len(next.filter(g.routable())) == 0 {
		return "", 0, Target{}, fmt.Errorf("%s target %q has no ready node", result, name)
	}
	return result, pHard, next, nil
}

// askClassifier sends one 1-token chat completion to b and returns
// P(B) / (P(A) + P(B)) and P(A) + P(B) from the first token's top_logprobs.
func (g *Gateway) askClassifier(ctx context.Context, b Backend, text string) (pHard, mass float64, err error) {
	body := mustJSON(map[string]any{
		"model": b.Model,
		"messages": []map[string]string{
			{"role": "system", "content": routerSystem},
			{"role": "user", "content": "<request>" + text + "</request>\nIs this request EASY (A) or HARD (B)? Answer:"},
		},
		"max_tokens":           1,
		"temperature":          0,
		"logprobs":             true,
		"top_logprobs":         10,
		"cache_prompt":         true,
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
	})
	g.addInflight(b.NodeID, 1)
	defer g.addInflight(b.NodeID, -1)
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, b.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	up.Header.Set("Content-Type", "application/json")
	setUpstreamAuth(up, b)
	resp, err := g.client.Do(up)
	if err != nil {
		return 0, 0, fmt.Errorf("classifier %s: %w", b.NodeID, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, 0, fmt.Errorf("classifier %s: %w", b.NodeID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("classifier %s: HTTP %d: %s", b.NodeID, resp.StatusCode, clip(string(bytes.TrimSpace(raw)), 200))
	}
	return hardProbability(raw)
}

// hardProbability reads P(B) / (P(A) + P(B)) and P(A) + P(B) from an
// OpenAI-style chat completion with logprobs (llama-server's format).
func hardProbability(raw []byte) (pHard, mass float64, err error) {
	type logprob struct {
		Token   string  `json:"token"`
		Logprob float64 `json:"logprob"`
	}
	var resp struct {
		Choices []struct {
			Logprobs struct {
				Content []struct {
					logprob
					TopLogprobs []logprob `json:"top_logprobs"`
				} `json:"content"`
			} `json:"logprobs"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, 0, fmt.Errorf("classifier response: %w", err)
	}
	if len(resp.Choices) == 0 || len(resp.Choices[0].Logprobs.Content) == 0 {
		return 0, 0, errors.New("classifier returned no logprobs (does the node's server support them?)")
	}
	first := resp.Choices[0].Logprobs.Content[0]
	tops := first.TopLogprobs
	if len(tops) == 0 {
		tops = []logprob{first.logprob}
	}
	var pA, pB float64
	for _, t := range tops {
		switch strings.ToUpper(strings.TrimSpace(t.Token)) {
		case "A":
			pA += math.Exp(t.Logprob)
		case "B":
			pB += math.Exp(t.Logprob)
		}
	}
	if pA+pB == 0 {
		return 0, 0, fmt.Errorf("classifier answered %q, not A or B", first.Token)
	}
	return pB / (pA + pB), pA + pB, nil
}

// requestText returns the text to classify: the last user message of a chat
// request, or the prompt of a completion, clipped to routerMaxText.
func requestText(meta requestMeta) string {
	var text string
	for i := len(meta.Messages) - 1; i >= 0 && text == ""; i-- {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(meta.Messages[i], &m) != nil || m.Role != "user" {
			continue
		}
		text = contentText(m.Content)
	}
	if text == "" && len(meta.Messages) == 0 {
		_ = json.Unmarshal(meta.Prompt, &text)
	}
	return clip(strings.TrimSpace(text), routerMaxText)
}

// contentText flattens a message content: a string, or an array of parts
// whose text parts are joined.
func contentText(c json.RawMessage) string {
	var s string
	if json.Unmarshal(c, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(c, &parts)
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}
