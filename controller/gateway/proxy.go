package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// BackendSource returns the currently usable backends (ready, ACTIVE nodes).
type BackendSource func() []Backend

type Config struct {
	UpstreamTimeout time.Duration // whole request, including generation; see SetUpstreamTimeout
	MaxAttempts     int           // distinct backends tried per request
	Cooldown        time.Duration // how long a failed backend is avoided
	Usage           UsageRecorder // optional usage accounting
}

// UsageEvent is one outcome for usage accounting: a finished request, or a
// failed attempt on a node (Code CodeUpstreamError) that may be retried.
type UsageEvent struct {
	Principal string
	NodeID    string // empty when no node served the request
	Model     string
	Code      string // HTTP status, "aborted" or CodeUpstreamError

	PromptTokens, CachedTokens, CompletionTokens int
	GenTPS                                       float64 // generation tokens/s reported by the node, 0 = unknown
}

// CodeUpstreamError marks a failed attempt against a node. The client
// request continues on another node, or ends with a separate 502 event.
const CodeUpstreamError = "upstream_error"

// UsageRecorder receives usage events. It is called on the request path, so
// implementations must be fast and safe for concurrent use.
type UsageRecorder interface {
	Record(UsageEvent)
}

type noUsage struct{}

func (noUsage) Record(UsageEvent) {}

type Gateway struct {
	auth      Authenticator
	picker    atomic.Pointer[Picker]
	targets   atomic.Pointer[Targets]
	modelInfo atomic.Pointer[ModelInfoFunc] // catalog metadata for Ollama responses (ADR-017)
	timeout   atomic.Int64                  // upstream timeout, ns
	backends  BackendSource
	cfg       Config
	log       *slog.Logger
	client    *http.Client
	now       func() time.Time

	prewarmTimeout time.Duration

	mu       sync.Mutex
	inflight map[string]int
	cooldown map[string]time.Time
	attempts map[string]map[*attempt]struct{} // in-flight attempts per node, for Reap

	mRequests  *prometheus.CounterVec
	mDuration  *prometheus.HistogramVec
	mTTFB      *prometheus.HistogramVec
	mInflight  *prometheus.GaugeVec
	mUpstream  *prometheus.CounterVec
	mRejected  *prometheus.CounterVec
	mTokens    *prometheus.CounterVec
	mGenTPS    *prometheus.GaugeVec
	mPromptTPS *prometheus.GaugeVec
	mTargets   *prometheus.CounterVec
}

func New(auth Authenticator, picker Picker, backends BackendSource, cfg Config, reg prometheus.Registerer, log *slog.Logger) *Gateway {
	buckets := []float64{.1, .25, .5, 1, 2, 5, 10, 20, 40, 80}
	g := &Gateway{
		auth: auth, backends: backends, cfg: cfg,
		log: log.With("component", "gateway"), now: time.Now,
		client: &http.Client{Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     60 * time.Second,
		}},
		inflight: map[string]int{},
		cooldown: map[string]time.Time{},
		attempts: map[string]map[*attempt]struct{}{},
		mRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "phoneborg_gateway_requests_total", Help: "Proxied inference requests."},
			[]string{"model", "node_id", "code", "principal"}),
		mDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "phoneborg_gateway_request_duration_seconds", Help: "End-to-end request latency.", Buckets: buckets},
			[]string{"model"}),
		mTTFB: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "phoneborg_gateway_time_to_first_byte_seconds", Help: "Time until the backend's first response byte.", Buckets: buckets},
			[]string{"model"}),
		mInflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "phoneborg_gateway_inflight_requests", Help: "Requests in flight per node."}, []string{"node_id"}),
		mUpstream: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "phoneborg_gateway_upstream_errors_total", Help: "Failed attempts against a backend (retried elsewhere if possible)."},
			[]string{"node_id"}),
		mRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "phoneborg_gateway_rejected_total", Help: "Requests rejected before reaching a backend."}, []string{"reason"}),
		mTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "phoneborg_gateway_tokens_total", Help: "Tokens from backend usage reports. kind=prompt (all prompt tokens), prompt_cached (reused from cache), completion."},
			[]string{"model", "node_id", "kind", "principal"}),
		mGenTPS: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "phoneborg_node_generation_tokens_per_second", Help: "Generation speed of the node's last request."}, []string{"node_id"}),
		mPromptTPS: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "phoneborg_node_prompt_tokens_per_second", Help: "Prompt processing speed of the node's last request."}, []string{"node_id"}),
		mTargets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "phoneborg_gateway_target_requests_total", Help: "Inference requests by resolved target: auto, pool/<name>, node/<alias-or-id> or model (ADR-014)."},
			[]string{"target"}),
	}
	if g.cfg.Usage == nil {
		g.cfg.Usage = noUsage{}
	}
	g.SetPicker(picker)
	g.SetUpstreamTimeout(cfg.UpstreamTimeout)
	g.prewarmTimeout = PrewarmTimeout
	reg.MustRegister(g.mRequests, g.mDuration, g.mTTFB, g.mInflight, g.mUpstream, g.mRejected, g.mTokens, g.mGenTPS, g.mPromptTPS, g.mTargets)
	// Export known reasons at 0 so the first rejection shows up in rate()/increase().
	for _, reason := range []string{"unauthorized", "bad_request", "model_not_found", "node_unavailable", "backends_failed", "context_too_large", "remote_requires_api_key"} {
		g.mRejected.WithLabelValues(reason)
	}
	for _, target := range []string{KindAuto, KindModel} {
		g.mTargets.WithLabelValues(target)
	}
	return g
}

// SetPicker switches the routing policy. Requests already routed are not
// affected; safe to call while serving.
func (g *Gateway) SetPicker(p Picker) { g.picker.Store(&p) }

// Picker returns the current routing policy.
func (g *Gateway) Picker() Picker { return *g.picker.Load() }

// SetUpstreamTimeout changes the timeout of requests that start afterwards.
func (g *Gateway) SetUpstreamTimeout(d time.Duration) { g.timeout.Store(int64(d)) }

// UpstreamTimeout returns the timeout applied to new requests.
func (g *Gateway) UpstreamTimeout() time.Duration { return time.Duration(g.timeout.Load()) }

// Inflight returns the number of in-flight requests per node.
func (g *Gateway) Inflight() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]int, len(g.inflight))
	for k, v := range g.inflight {
		if v > 0 {
			out[k] = v
		}
	}
	return out
}

// routable returns the backends that may take new requests.
func (g *Gateway) routable() []Backend {
	var out []Backend
	for _, b := range g.backends() {
		if !b.Drained {
			out = append(out, b)
		}
	}
	return out
}

func (g *Gateway) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", g.handleProxy)
	mux.HandleFunc("POST /v1/completions", g.handleProxy)
	mux.HandleFunc("GET /v1/models", g.handleModels)
}

// requestMeta is the part of an OpenAI request body the gateway looks at.
type requestMeta struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	Messages []json.RawMessage `json:"messages"`
	Tools    json.RawMessage   `json:"tools"`
	Prompt   json.RawMessage   `json:"prompt"` // /v1/completions
}

// affinityKey hashes the prompt prefix that repeats across a session's
// requests: the first message (usually the system prompt) plus the tool
// definitions. Agents like opencode resend ~10k identical tokens there on
// every turn; routing them to the same node lets llama-server reuse its cache.
func (m requestMeta) affinityKey() string {
	h := sha256.New()
	switch {
	case len(m.Messages) > 0:
		h.Write(m.Messages[0])
		h.Write(m.Tools)
	case len(m.Prompt) > 0:
		h.Write(m.Prompt[:min(len(m.Prompt), 1024)])
	default:
		return ""
	}
	h.Write([]byte(m.Model))
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// errContextTooSmall marks a node that rejected the prompt as longer than its
// context window. The request is retried elsewhere without a cooldown.
var errContextTooSmall = fmt.Errorf("%w: prompt exceeds node context", errRetryable)

// estimateTokens approximates prompt tokens from the JSON body size. English
// text and JSON average roughly 4 bytes per token for common tokenizers; the
// estimate only steers routing, llama-server still enforces the real limit.
func estimateTokens(body []byte) int { return len(body) / 4 }

// maxContext is the largest known context among the backends, or 0 if no
// backend reports one.
func maxContext(all []Backend) int {
	m := 0
	for _, b := range all {
		if b.CtxSize > m {
			m = b.CtxSize
		}
	}
	return m
}

// errNodeLost cancels attempts on a node that left the backend set.
var errNodeLost = errors.New("node left the ready set")

type attempt struct{ cancel context.CancelCauseFunc }

// Reap cancels in-flight attempts on nodes that are no longer usable (e.g.
// SUSPECT after missed heartbeats), so requests stuck on a frozen phone are
// retried elsewhere instead of waiting for UpstreamTimeout. Call periodically.
func (g *Gateway) Reap() {
	live := map[string]bool{}
	for _, b := range g.backends() { // drained nodes stay live: their requests finish
		live[b.NodeID] = true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for node, as := range g.attempts {
		if live[node] {
			continue
		}
		for a := range as {
			a.cancel(errNodeLost)
		}
		if len(as) > 0 {
			g.log.Warn("cancelled in-flight requests on lost node", "node_id", node, "count", len(as))
		}
	}
}

func (g *Gateway) track(nodeID string, a *attempt, add bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if add {
		if g.attempts[nodeID] == nil {
			g.attempts[nodeID] = map[*attempt]struct{}{}
		}
		g.attempts[nodeID][a] = struct{}{}
	} else {
		delete(g.attempts[nodeID], a)
	}
}

// errRetryable marks failures that happened before anything was sent to the
// client, so another backend may still serve the request.
var errRetryable = errors.New("retryable upstream failure")

func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get("X-Request-Id")
	if reqID == "" {
		reqID = newID()
	}
	w.Header().Set("X-Request-Id", reqID)
	log := g.log.With("request_id", reqID, "path", r.URL.Path)

	principal, err := g.auth.Authenticate(r)
	if err != nil {
		g.rejectUnauthorized(w, err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		g.mRejected.WithLabelValues("bad_request").Inc()
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "", "cannot read body: "+err.Error())
		return
	}
	var meta requestMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		g.mRejected.WithLabelValues("bad_request").Inc()
		g.cfg.Usage.Record(UsageEvent{Principal: principal.Name, Code: "400"})
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "", "invalid JSON: "+err.Error())
		return
	}

	tgt, err := g.Resolve(meta.Model)
	if err != nil {
		g.mRejected.WithLabelValues("model_not_found").Inc()
		g.cfg.Usage.Record(UsageEvent{Principal: principal.Name, Model: meta.Model, Code: strconv.Itoa(http.StatusNotFound)})
		openAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", fmt.Sprintf("no pool or node %q", meta.Model))
		return
	}
	g.mTargets.WithLabelValues(tgt.Label).Inc()

	all := tgt.filter(g.routable())
	if tgt.Node != "" && len(all) == 0 {
		g.mRejected.WithLabelValues("node_unavailable").Inc()
		g.cfg.Usage.Record(UsageEvent{Principal: principal.Name, Model: meta.Model, Code: strconv.Itoa(http.StatusServiceUnavailable)})
		openAIError(w, http.StatusServiceUnavailable, "server_error", "node_unavailable",
			fmt.Sprintf("%s is not ready (offline, drained, loading or switching models)", meta.Model))
		return
	}
	if len(all) == 0 {
		g.mRejected.WithLabelValues("model_not_found").Inc()
		code, msg := http.StatusNotFound, fmt.Sprintf("model %q is not served by any ready node", meta.Model)
		switch {
		case tgt.Nodes != nil: // a pool without eligible members
			code, msg = http.StatusServiceUnavailable, fmt.Sprintf("%s has no eligible ready node", meta.Model)
		case len(g.routable()) == 0:
			code, msg = http.StatusServiceUnavailable, "no ready nodes"
		}
		g.cfg.Usage.Record(UsageEvent{Principal: principal.Name, Model: meta.Model, Code: strconv.Itoa(code)})
		openAIError(w, code, "invalid_request_error", "model_not_found", msg)
		return
	}

	est := estimateTokens(body)
	if max := maxContext(all); max > 0 && est > max {
		g.mRejected.WithLabelValues("context_too_large").Inc()
		g.cfg.Usage.Record(UsageEvent{Principal: principal.Name, Model: meta.Model, Code: strconv.Itoa(http.StatusBadRequest)})
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "context_length_exceeded",
			fmt.Sprintf("request is about %d tokens, larger than the biggest node context (%d tokens); raise the nodes' -ctx-size", est, max))
		return
	}

	picker := tgt.Picker
	if picker == nil {
		picker = g.Picker()
	}
	maxAttempts := g.cfg.MaxAttempts
	if tgt.Node != "" {
		maxAttempts = 1 // node targets never fail over
	}
	tried := map[string]bool{}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Re-read backends each attempt: nodes may have joined or left
		// (e.g. reaped as SUSPECT) since the request arrived.
		cands := g.candidates(tgt.filter(g.routable()), tried, est)
		if len(cands) == 0 {
			break
		}
		b := cands[0] // node targets bypass the picker
		if tgt.Node == "" {
			b = picker.Pick(Request{Model: meta.Model, AffinityKey: meta.affinityKey(), PromptBytes: len(body)}, cands, g.inflightOf)
		}
		tried[b.NodeID] = true
		err := g.forward(w, r, b, body, meta.Stream, principal, reqID)
		if err == nil {
			return
		}
		g.mUpstream.WithLabelValues(b.NodeID).Inc()
		if !errors.Is(err, errContextTooSmall) { // the node is healthy, the prompt just did not fit
			g.markDown(b.NodeID)
		}
		if errors.Is(err, errRetryable) { // otherwise forward already recorded the outcome
			g.cfg.Usage.Record(UsageEvent{Principal: principal.Name, NodeID: b.NodeID, Model: b.Model, Code: CodeUpstreamError})
		}
		log.Warn("backend failed", "node_id", b.NodeID, "attempt", attempt, "err", err)
		if !errors.Is(err, errRetryable) {
			return // response already started; nothing more we can do
		}
	}
	g.mRejected.WithLabelValues("backends_failed").Inc()
	g.cfg.Usage.Record(UsageEvent{Principal: principal.Name, Model: meta.Model, Code: strconv.Itoa(http.StatusBadGateway)})
	openAIError(w, http.StatusBadGateway, "server_error", "backends_failed", "all attempted nodes failed")
}

// rejectUnauthorized writes the OpenAI-shaped error for a failed
// Authenticate call and counts it, distinguishing ADR-017's "local" access
// mode (403, a distinct reason) from an invalid or missing API key (401).
func (g *Gateway) rejectUnauthorized(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrRemoteRequiresKey) {
		g.mRejected.WithLabelValues("remote_requires_api_key").Inc()
		openAIError(w, http.StatusForbidden, "access_denied", "remote_requires_api_key", err.Error())
		return
	}
	g.mRejected.WithLabelValues("unauthorized").Inc()
	openAIError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", err.Error())
}

func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, b Backend, body []byte, stream bool, p Principal, reqID string) error {
	g.addInflight(b.NodeID, 1)
	defer g.addInflight(b.NodeID, -1)
	start := g.now()

	cctx, cancelCause := context.WithCancelCause(r.Context())
	defer cancelCause(nil)
	a := &attempt{cancel: cancelCause}
	g.track(b.NodeID, a, true)
	defer g.track(b.NodeID, a, false)
	ctx, cancel := context.WithTimeout(cctx, g.UpstreamTimeout())
	defer cancel()
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, b.URL+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", errRetryable, err)
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("X-Request-Id", reqID)
	resp, err := g.client.Do(up)
	if err != nil {
		if r.Context().Err() != nil {
			g.mRequests.WithLabelValues(b.Model, b.NodeID, "499", p.Name).Inc()
			g.cfg.Usage.Record(UsageEvent{Principal: p.Name, NodeID: b.NodeID, Model: b.Model, Code: "499"})
			return nil // client went away
		}
		return fmt.Errorf("%w: %v", errRetryable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 { // e.g. 503 while the model is still loading
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("%w: HTTP %d: %s", errRetryable, resp.StatusCode, bytes.TrimSpace(msg))
	}
	var src io.Reader = resp.Body
	if resp.StatusCode == http.StatusBadRequest {
		// llama-server answers 400 when the prompt exceeds its context; that
		// node is fine, another one with a larger context may serve it.
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if bytes.Contains(peek, []byte("exceeds the available context size")) {
			return fmt.Errorf("%w: %s", errContextTooSmall, bytes.TrimSpace(peek))
		}
		src = io.MultiReader(bytes.NewReader(peek), resp.Body)
	}

	for k, vs := range resp.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Keep-Alive", "Transfer-Encoding", "Content-Length":
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-PhoneBorg-Node", b.NodeID)
	// From here on bytes reach the client, so a lost node aborts the request.
	w.WriteHeader(resp.StatusCode)

	// Copy, flushing each chunk so SSE streams reach the client immediately.
	// Keep the tail of the body to read usage/timings afterwards.
	rc := http.NewResponseController(w)
	tail := &tailBuffer{max: 64 << 10}
	buf := make([]byte, 16<<10)
	first := true
	var copyErr error
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if first {
				g.mTTFB.WithLabelValues(b.Model).Observe(g.now().Sub(start).Seconds())
				first = false
			}
			tail.Write(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				copyErr = werr
				break
			}
			_ = rc.Flush()
		}
		if rerr != nil {
			if rerr != io.EOF {
				copyErr = rerr
			}
			break
		}
	}

	code := strconv.Itoa(resp.StatusCode)
	if copyErr != nil {
		code = "aborted"
	}
	g.mRequests.WithLabelValues(b.Model, b.NodeID, code, p.Name).Inc()
	g.mDuration.WithLabelValues(b.Model).Observe(g.now().Sub(start).Seconds())
	ev := UsageEvent{Principal: p.Name, NodeID: b.NodeID, Model: b.Model, Code: code}
	if resp.StatusCode == http.StatusOK {
		if u, ok := parseUsage(tail.Bytes(), stream); ok {
			ev.PromptTokens, ev.CachedTokens, ev.CompletionTokens, ev.GenTPS = u.PromptTokens, u.CachedTokens, u.CompletionTokens, u.GenTPS
			g.mTokens.WithLabelValues(b.Model, b.NodeID, "prompt", p.Name).Add(float64(u.PromptTokens))
			g.mTokens.WithLabelValues(b.Model, b.NodeID, "completion", p.Name).Add(float64(u.CompletionTokens))
			g.mTokens.WithLabelValues(b.Model, b.NodeID, "prompt_cached", p.Name).Add(float64(u.CachedTokens))
			if u.GenTPS > 0 {
				g.mGenTPS.WithLabelValues(b.NodeID).Set(u.GenTPS)
			}
			if u.PromptTPS > 0 {
				g.mPromptTPS.WithLabelValues(b.NodeID).Set(u.PromptTPS)
			}
		}
	}
	g.cfg.Usage.Record(ev)
	g.log.Info("request done", "request_id", reqID, "node_id", b.NodeID, "model", b.Model,
		"principal", p.Name, "status", code, "stream", stream, "duration_ms", g.now().Sub(start).Milliseconds())
	if copyErr != nil {
		return fmt.Errorf("stream copy: %w", copyErr) // not retryable: bytes already sent
	}
	return nil
}

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, err := g.auth.Authenticate(r); err != nil {
		g.rejectUnauthorized(w, err)
		return
	}
	out := struct {
		Object string       `json:"object"`
		Data   []ModelEntry `json:"data"`
	}{Object: "list", Data: g.modelEntries()}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// modelEntries lists served models, then virtual models (ADR-014): "auto",
// then pools and aliased nodes from the Targets resolver. Shared by
// GET /v1/models and the Ollama-compatible GET /api/tags (ADR-017), so both
// see the same routing state.
func (g *Gateway) modelEntries() []ModelEntry {
	seen := map[string]int{}
	routable := g.routable()
	for _, b := range routable {
		seen[b.Model]++
	}
	out := []ModelEntry{}
	for m, n := range seen {
		out = append(out, ModelEntry{ID: m, Object: "model", OwnedBy: "phoneborg", Kind: KindModel, Nodes: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	out = append(out, ModelEntry{ID: KindAuto, Object: "model", OwnedBy: "phoneborg", Kind: KindAuto, Nodes: len(routable)})
	if t := g.targets.Load(); t != nil && *t != nil {
		out = append(out, (*t).Models()...)
	}
	return out
}

// candidates returns untried backends whose context fits, preferring ones not in
// cooldown; if every one is cooling down, they are tried anyway.
func (g *Gateway) candidates(all []Backend, tried map[string]bool, estTokens int) []Backend {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	var healthy, cooling []Backend
	for _, b := range all {
		if tried[b.NodeID] || (b.CtxSize > 0 && estTokens > b.CtxSize) {
			continue
		}
		if now.Before(g.cooldown[b.NodeID]) {
			cooling = append(cooling, b)
		} else {
			healthy = append(healthy, b)
		}
	}
	if len(healthy) > 0 {
		return healthy
	}
	return cooling
}

func (g *Gateway) markDown(nodeID string) {
	g.mu.Lock()
	g.cooldown[nodeID] = g.now().Add(g.cfg.Cooldown)
	g.mu.Unlock()
}

func (g *Gateway) addInflight(nodeID string, d int) {
	g.mu.Lock()
	g.inflight[nodeID] += d
	n := g.inflight[nodeID]
	g.mu.Unlock()
	g.mInflight.WithLabelValues(nodeID).Set(float64(n))
}

func (g *Gateway) inflightOf(nodeID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflight[nodeID]
}

type usage struct {
	PromptTokens, CompletionTokens int
	CachedTokens                   int // prompt tokens reused from the node's cache
	PromptTPS, GenTPS              float64
}

// parseUsage reads OpenAI `usage` and llama.cpp `timings` from a response
// body, or from the last SSE data event that carries them.
func parseUsage(body []byte, stream bool) (usage, bool) {
	docs := [][]byte{body}
	if stream {
		docs = nil
		for _, line := range bytes.Split(body, []byte("\n")) {
			if d, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data: ")); ok && bytes.HasPrefix(d, []byte("{")) {
				docs = append([][]byte{d}, docs...) // newest first
			}
		}
	}
	for _, d := range docs {
		var v struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Timings *struct {
				CacheN   int     `json:"cache_n"`
				PromptN  int     `json:"prompt_n"`
				PromptPS float64 `json:"prompt_per_second"`
				PredN    int     `json:"predicted_n"`
				PredPS   float64 `json:"predicted_per_second"`
			} `json:"timings"`
		}
		if json.Unmarshal(d, &v) != nil || (v.Usage == nil && v.Timings == nil) {
			continue
		}
		var u usage
		if v.Usage != nil {
			u.PromptTokens, u.CompletionTokens = v.Usage.PromptTokens, v.Usage.CompletionTokens
		}
		if t := v.Timings; t != nil {
			u.PromptTPS, u.GenTPS = t.PromptPS, t.PredPS
			u.CachedTokens = t.CacheN
			if v.Usage == nil {
				u.PromptTokens, u.CompletionTokens = t.CacheN+t.PromptN, t.PredN
			}
		}
		return u, true
	}
	return usage{}, false
}

type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
}

func (t *tailBuffer) Bytes() []byte { return t.buf }

func openAIError(w http.ResponseWriter, status int, typ, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	e := map[string]any{"message": msg, "type": typ}
	if code != "" {
		e["code"] = code
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": e})
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
