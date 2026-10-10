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
// request for "auto" is first sorted into one of the configured classes
// (by default DefaultRouterClasses: easy or hard chat, writing, coding,
// reasoning) by one phone, and then served by that class's target. The
// classifier generates a single token: the gateway reads the probabilities
// of the class letters from llama-server's top_logprobs, so the cost is
// prompt processing only. Any failure (no classifier node, timeout, no
// usable logprobs, an unknown target) falls back to plain "auto": the
// router can only make routing smarter, never make a request fail.

// RouterClass is one class the classifier can choose. The operator may
// add, change and remove classes; each gets the answer letter of its
// position (A, B, ...).
type RouterClass struct {
	Name        string   `json:"name"` // metric label, X-Phoneborg-Route value
	Description string   `json:"description"`
	Examples    []string `json:"examples,omitempty"` // requests of this class, shown to the classifier
	// Target is where the class's requests go ("auto", "pool/<name>",
	// "node/<alias>" or a model id); "" means "auto".
	Target string `json:"target"`
}

// MaxRouterClasses caps the classes: one answer letter each, and every
// class makes the cached classifier prompt longer.
const MaxRouterClasses = 12

// RouterLetter is the classifier's answer token for the i-th class.
func RouterLetter(i int) string { return string(rune('A' + i)) }

// DefaultRouterClasses are the classes until the operator changes them;
// this prompt and these examples were tested together (OPERATIONS.md).
func DefaultRouterClasses() []RouterClass {
	return []RouterClass{
		{Name: "easy_chat", Description: "greeting, chit-chat or a short factual question",
			Examples: []string{"Hello! How are you today?", `Who wrote "Pride and Prejudice"?`}},
		{Name: "easy_writing", Description: "a short text, a simple rewrite, a translation or a short summary",
			Examples: []string{`Translate "thank you very much" into Spanish.`, "Make this sentence more polite: send me the file now."}},
		{Name: "hard_writing", Description: "a long, creative or technical text: a story, an article, documentation, a detailed report",
			Examples: []string{"Write a 2000-word short story about a lighthouse keeper.", "Write the user guide for our backup tool, with examples."}},
		{Name: "easy_coding", Description: "a small code snippet, a one-liner or explaining short code",
			Examples: []string{"How do I reverse a list in Python?", "What does `git stash pop` do?"}},
		{Name: "hard_coding", Description: "implementing, debugging or reviewing non-trivial code, or software design",
			Examples: []string{"Implement an LRU cache in Go with O(1) operations and tests.", "My Rust service deadlocks under load; here is the code, find the bug."}},
		{Name: "hard_reasoning", Description: "math, proofs, multi-step reasoning, analysis or planning",
			Examples: []string{"Prove that the square root of 2 is irrational.", "Plan the migration of a 3-node Postgres cluster to Kubernetes."}},
	}
}

// RouterConfig is the semantic router's runtime configuration.
type RouterConfig struct {
	Enabled bool
	// Classifier is the routing target that classifies, e.g. "node/pixel".
	Classifier string
	Classes    []RouterClass // in letter order
	// Timeout bounds the classification; on expiry the request is plain "auto".
	Timeout time.Duration
}

// DefaultRouterConfig returns the router's configuration until the
// operator changes it: disabled, with the default classes.
func DefaultRouterConfig() RouterConfig {
	return RouterConfig{Classes: DefaultRouterClasses(), Timeout: 20 * time.Second}
}

// routerState is a configuration with the classifier prompt built for it.
type routerState struct {
	cfg     RouterConfig
	system  string // constant per configuration, so the classifier caches it
	letters string // "A, B, ... or F"
}

// RouteFallback is the decision result when the request is served as plain
// "auto"; the other results are class names.
const RouteFallback = "fallback"

// routerMaxText caps the request text shown to the classifier: prompt
// processing is what a decision costs, and the start of a request says
// enough about its kind.
const routerMaxText = 2000

// routerMinMass is the least probability the classifier must put on the
// class letters together. Below it the model did not answer the question
// (a small model may start doing the request instead), and its split
// means nothing.
const routerMinMass = 0.5

// buildRouterSystem returns the classifier's system prompt for classes and
// the list of their letters ("A, B or C"). The request text is not in it,
// so the classifier node caches it and processes only the request per
// decision.
func buildRouterSystem(classes []RouterClass) (string, string) {
	var b, ex strings.Builder
	var letters []string
	b.WriteString("You are a request classifier. You never answer or carry out the request; you only say which class it belongs to.\n")
	for i, c := range classes {
		l := RouterLetter(i)
		fmt.Fprintf(&b, "%s = %s: %s.\n", l, strings.ToUpper(c.Name), strings.TrimSuffix(c.Description, "."))
		for _, e := range c.Examples {
			fmt.Fprintf(&ex, "<request>%s</request> Answer: %s\n", e, l)
		}
		letters = append(letters, l)
	}
	list := letters[0]
	if n := len(letters); n > 1 {
		list = strings.Join(letters[:n-1], ", ") + " or " + letters[n-1]
	}
	if ex.Len() > 0 {
		fmt.Fprintf(&b, "\n%s", ex.String())
	}
	fmt.Fprintf(&b, "\nReply with one letter only: %s.", list)
	return b.String(), list
}

// SetRouter installs the semantic router configuration. Callers validate
// it (names, descriptions, at most MaxRouterClasses classes).
func (g *Gateway) SetRouter(c RouterConfig) {
	c.Classes = append([]RouterClass(nil), c.Classes...)
	st := &routerState{cfg: c}
	if len(c.Classes) > 0 {
		st.system, st.letters = buildRouterSystem(c.Classes)
	}
	for _, cl := range c.Classes {
		g.mRouter.WithLabelValues(cl.Name)
	}
	g.router.Store(st)
}

// Router returns the semantic router configuration.
func (g *Gateway) Router() RouterConfig {
	c := g.router.Load().cfg
	c.Classes = append([]RouterClass(nil), c.Classes...)
	return c
}

// routeAuto classifies an "auto" request and returns the target it should
// go to; on any failure it returns tgt unchanged.
func (g *Gateway) routeAuto(w http.ResponseWriter, r *http.Request, tgt Target, meta requestMeta, reqID string) Target {
	st := g.router.Load()
	if !st.cfg.Enabled {
		return tgt
	}
	log := g.log.With("request_id", reqID)
	start := g.now()
	class, p, next, err := g.classify(r.Context(), st, meta)
	g.mRouterSeconds.Observe(g.now().Sub(start).Seconds())
	result := class
	if err != nil {
		result, next = RouteFallback, tgt
		log.Warn("router fallback to auto", "err", err)
	} else {
		log.Info("router decision", "route", class, "p", math.Round(p*1000)/1000, "target", next.Label,
			"duration_ms", g.now().Sub(start).Milliseconds())
	}
	g.mRouter.WithLabelValues(result).Inc()
	w.Header().Set("X-Phoneborg-Route", result)
	return next
}

// classify asks the classifier for the request's class and resolves its
// target. p is the class's share of the probability on all class letters.
func (g *Gateway) classify(ctx context.Context, st *routerState, meta requestMeta) (string, float64, Target, error) {
	cfg := st.cfg
	if len(cfg.Classes) == 0 {
		return "", 0, Target{}, errors.New("no classes configured")
	}
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
	probs, err := g.askClassifier(ctx, b, st, text)
	if err != nil {
		return "", 0, Target{}, err
	}
	i, p, mass := bestClass(probs, len(cfg.Classes))
	if mass < routerMinMass {
		return "", 0, Target{}, fmt.Errorf("classifier %s put only %.2f on the class letters (model unsuitable as a classifier?)", b.NodeID, mass)
	}
	class, name := cfg.Classes[i].Name, cfg.Classes[i].Target
	if name == "" {
		name = KindAuto
	}
	next, err := g.Resolve(name)
	if err != nil {
		return "", 0, Target{}, fmt.Errorf("%s target %q: %w", class, name, err)
	}
	if len(next.filter(g.routable())) == 0 {
		return "", 0, Target{}, fmt.Errorf("%s target %q has no ready node", class, name)
	}
	return class, p, next, nil
}

// bestClass returns the index of the most probable of n classes, its share
// of the probability on all class letters, and that total.
func bestClass(probs map[string]float64, n int) (i int, p, mass float64) {
	var best float64
	for j := 0; j < n; j++ {
		q := probs[RouterLetter(j)]
		mass += q
		if q > best {
			i, best = j, q
		}
	}
	if mass == 0 {
		return 0, 0, 0
	}
	return i, best / mass, mass
}

// askClassifier sends one 1-token chat completion to b and returns the
// probability of each answer letter from the first token's top_logprobs.
func (g *Gateway) askClassifier(ctx context.Context, b Backend, st *routerState, text string) (map[string]float64, error) {
	body := mustJSON(map[string]any{
		"model": b.Model,
		"messages": []map[string]string{
			{"role": "system", "content": st.system},
			{"role": "user", "content": "<request>" + text + "</request>\nWhich class is this request (" + st.letters + ")? Answer:"},
		},
		"max_tokens":           1,
		"temperature":          0,
		"logprobs":             true,
		"top_logprobs":         20,
		"cache_prompt":         true,
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
	})
	g.addInflight(b.NodeID, 1)
	defer g.addInflight(b.NodeID, -1)
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, b.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	up.Header.Set("Content-Type", "application/json")
	setUpstreamAuth(up, b)
	resp, err := g.client.Do(up)
	if err != nil {
		return nil, fmt.Errorf("classifier %s: %w", b.NodeID, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("classifier %s: %w", b.NodeID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("classifier %s: HTTP %d: %s", b.NodeID, resp.StatusCode, clip(string(bytes.TrimSpace(raw)), 200))
	}
	return letterProbs(raw)
}

// letterProbs reads the probability of each single-letter answer token
// (upper-cased, spaces trimmed) from an OpenAI-style chat completion with
// logprobs (llama-server's format).
func letterProbs(raw []byte) (map[string]float64, error) {
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
		return nil, fmt.Errorf("classifier response: %w", err)
	}
	if len(resp.Choices) == 0 || len(resp.Choices[0].Logprobs.Content) == 0 {
		return nil, errors.New("classifier returned no logprobs (does the node's server support them?)")
	}
	first := resp.Choices[0].Logprobs.Content[0]
	tops := first.TopLogprobs
	if len(tops) == 0 {
		tops = []logprob{first.logprob}
	}
	probs := map[string]float64{}
	for _, t := range tops {
		if l := strings.ToUpper(strings.TrimSpace(t.Token)); len(l) == 1 {
			probs[l] += math.Exp(t.Logprob)
		}
	}
	return probs, nil
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
