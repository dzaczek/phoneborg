package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

// External engine nodes (ADR-016): operator-added OpenAI-compatible servers
// (LM Studio, oMLX, Ollama, llama-server on a Mac or PC) that the gateway
// routes to like phones. They are not managed: no agent, no heartbeats, no
// placement. The controller polls GET {url}/v1/models for health and
// inventory.

// External states.
const (
	ExternalActive  = "ACTIVE"
	ExternalOffline = "OFFLINE"
)

const (
	// ExternalPrefix starts an external node's node id: "ext:<name>".
	ExternalPrefix = "ext:"
	// ExternalPollInterval is how often each external is checked.
	ExternalPollInterval = 10 * time.Second
	externalPollTimeout  = 5 * time.Second
	externalFailsOffline = 3 // consecutive failed checks before OFFLINE
	externalSelfTestTime = 120 * time.Second
	externalFileVersion  = 1
	maxExternalConc      = 1024
)

// externalSelfTestPrompt is the fixed self-test prompt; the answer is capped
// at 32 tokens.
const externalSelfTestPrompt = "Count from 1 to 50, separated by spaces."

var (
	ErrExternalUnknown = errors.New("unknown external node")
	ErrExternalName    = errors.New(`name must match ^[a-z0-9][a-z0-9-]{0,31}$, must not start with "pool" and must not be "auto"`)
	ErrExternalTaken   = errors.New("name is used by a node (as its alias or id)")
)

// ExternalSpec is the body of PUT /admin/external/{name}.
type ExternalSpec struct {
	URL string `json:"url"`
	// APIKey is sent upstream as the bearer token. nil keeps the current
	// key, "" removes it. It is never returned.
	APIKey         *string  `json:"api_key,omitempty"`
	Models         []string `json:"models,omitempty"`          // allowlist; empty = every discovered model
	MaxConcurrency int      `json:"max_concurrency,omitempty"` // 0 = 1
	CtxSize        int      `json:"ctx_size,omitempty"`        // tokens; 0 = unknown
	SpeedTPS       float64  `json:"speed_tps,omitempty"`       // operator hint, overrides self-tests; 0 = none
}

// ExternalSpeed is one model's self-test result.
type ExternalSpeed struct {
	GenTPS    float64   `json:"gen_tps,omitempty"`
	PromptTPS float64   `json:"prompt_tps,omitempty"`
	At        time.Time `json:"at"`
	Error     string    `json:"error,omitempty"`
}

// External is an external node as returned by the admin API.
type External struct {
	Name             string                   `json:"name"`
	NodeID           string                   `json:"node_id"`
	URL              string                   `json:"url"`
	HasAPIKey        bool                     `json:"has_api_key"`
	Models           []string                 `json:"models"`
	DiscoveredModels []string                 `json:"discovered_models"`
	MaxConcurrency   int                      `json:"max_concurrency"`
	CtxSize          int                      `json:"ctx_size"`
	SpeedTPS         float64                  `json:"speed_tps,omitempty"`
	State            string                   `json:"state"`
	LastCheck        time.Time                `json:"last_check,omitzero"`
	LastError        string                   `json:"last_error,omitempty"`
	Measured         map[string]ExternalSpeed `json:"measured"`
	Drained          bool                     `json:"drained"`
	Inflight         int                      `json:"inflight"`
	PinnedSessions   int                      `json:"pinned_sessions"`
}

// Externals is the response of GET /admin/external.
type Externals struct {
	External []External `json:"external"`
}

// ExternalConfig is one persisted external node, including its API key.
type ExternalConfig struct {
	Name           string                   `json:"name"`
	URL            string                   `json:"url"`
	APIKey         string                   `json:"api_key,omitempty"`
	Models         []string                 `json:"models,omitempty"`
	MaxConcurrency int                      `json:"max_concurrency"`
	CtxSize        int                      `json:"ctx_size,omitempty"`
	SpeedTPS       float64                  `json:"speed_tps,omitempty"`
	Measured       map[string]ExternalSpeed `json:"measured,omitempty"`
}

// ExternalOptions configures external nodes.
type ExternalOptions struct {
	File  string           // persists them, with API keys (mode 0600); "" = memory only
	State []ExternalConfig // initial state (see LoadExternal)
}

type externalFile struct {
	Version  int              `json:"version"`
	External []ExternalConfig `json:"external"`
}

// LoadExternal reads an external nodes file; a missing file is empty.
func LoadExternal(path string) ([]ExternalConfig, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f externalFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w (move it away to start without external nodes)", path, err)
	}
	if f.Version != externalFileVersion {
		return nil, fmt.Errorf("%s: unsupported version %d", path, f.Version)
	}
	for i, c := range f.External {
		if !validAlias(c.Name) {
			return nil, fmt.Errorf("%s: external %q: %w", path, c.Name, ErrExternalName)
		}
		if f.External[i].URL, err = normExternalURL(c.URL); err != nil {
			return nil, fmt.Errorf("%s: external %q: %w", path, c.Name, err)
		}
	}
	return f.External, nil
}

// normExternalURL validates a base URL and drops a trailing "/" or "/v1".
func normExternalURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New(`url must be http(s)://host:port, e.g. "http://host.docker.internal:1234"`)
	}
	s := strings.TrimRight(u.String(), "/")
	return strings.TrimRight(strings.TrimSuffix(s, "/v1"), "/"), nil
}

// extNode is one external node's config and live state.
type extNode struct {
	cfg        ExternalConfig
	state      string
	lastCheck  time.Time
	lastErr    string
	fails      int
	discovered []string
	drained    bool // kept in memory only, like phone drains
	testing    bool
}

// externals holds the external nodes.
type externals struct {
	path        string
	client      *http.Client
	log         *slog.Logger
	transitions *prometheus.CounterVec
	kick        chan struct{} // wakes run after a registration

	mu     sync.Mutex // also serialises saves
	byName map[string]*extNode
}

func newExternals(opts ExternalOptions, log *slog.Logger) *externals {
	e := &externals{
		path: opts.File, client: &http.Client{}, log: log.With("component", "external"),
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "phoneborg_external_state_transitions_total", Help: "External node state transitions (ADR-016)."},
			[]string{"from", "to"}),
		kick:   make(chan struct{}, 1),
		byName: map[string]*extNode{},
	}
	for _, c := range opts.State {
		if c.MaxConcurrency <= 0 {
			c.MaxConcurrency = 1
		}
		e.byName[c.Name] = &extNode{cfg: c, state: ExternalOffline}
	}
	return e
}

// save writes the configs, with API keys, to the file (mode 0600). Call
// with mu held.
func (e *externals) save() error {
	if e.path == "" {
		return nil
	}
	f := externalFile{Version: externalFileVersion, External: []ExternalConfig{}}
	for _, n := range e.sorted() {
		f.External = append(f.External, n.cfg)
	}
	data, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.path), 0o700); err != nil {
		return err
	}
	return gateway.WriteFileAtomic(e.path, data, 0o600)
}

// sorted returns the nodes by name. Call with mu held.
func (e *externals) sorted() []*extNode {
	out := make([]*extNode, 0, len(e.byName))
	for _, n := range e.byName {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cfg.Name < out[j].cfg.Name })
	return out
}

// has reports whether name is an external node.
func (e *externals) has(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.byName[name] != nil
}

// normExternalSpec validates a PUT body and fills defaults: the URL is
// normalised, max_concurrency 0 becomes 1, the allowlist is deduplicated.
func normExternalSpec(spec ExternalSpec) (ExternalSpec, error) {
	u, err := normExternalURL(spec.URL)
	if err != nil {
		return spec, err
	}
	spec.URL = u
	if spec.MaxConcurrency < 0 || spec.MaxConcurrency > maxExternalConc {
		return spec, fmt.Errorf("max_concurrency must be between 1 and %d (0 = 1)", maxExternalConc)
	}
	if spec.MaxConcurrency == 0 {
		spec.MaxConcurrency = 1
	}
	if spec.CtxSize < 0 || spec.SpeedTPS < 0 {
		return spec, errors.New("ctx_size and speed_tps must not be negative")
	}
	allow := []string{}
	for _, m := range spec.Models {
		if m = strings.TrimSpace(m); m == "" {
			return spec, errors.New("models: empty entry")
		}
		if !slices.Contains(allow, m) {
			allow = append(allow, m)
		}
	}
	spec.Models = allow
	return spec, nil
}

// put adds or replaces an external node's config (a spec checked by
// normExternalSpec) and reports whether it is new. Measurements and the
// drain flag are kept while the URL stays the same.
func (e *externals) put(name string, spec ExternalSpec) (bool, error) {
	u, allow := spec.URL, spec.Models
	e.mu.Lock()
	defer e.mu.Unlock()
	old := e.byName[name]
	n := &extNode{state: ExternalOffline, cfg: ExternalConfig{Name: name, URL: u, Models: allow,
		MaxConcurrency: spec.MaxConcurrency, CtxSize: spec.CtxSize, SpeedTPS: spec.SpeedTPS}}
	if spec.APIKey != nil {
		n.cfg.APIKey = *spec.APIKey
	} else if old != nil {
		n.cfg.APIKey = old.cfg.APIKey
	}
	if old != nil && old.cfg.URL == u {
		n.cfg.Measured, n.drained = old.cfg.Measured, old.drained
		n.state, n.lastCheck, n.lastErr, n.fails = old.state, old.lastCheck, old.lastErr, old.fails
		n.discovered = filterModels(old.discovered, allow)
	}
	e.byName[name] = n
	select {
	case e.kick <- struct{}{}:
	default:
	}
	return old == nil, e.save()
}

// remove deletes an external node.
func (e *externals) remove(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.byName[name] == nil {
		return ErrExternalUnknown
	}
	delete(e.byName, name)
	return e.save()
}

// setDrained drains or undrains an external node (not persisted).
func (e *externals) setDrained(name string, drained bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := e.byName[name]
	if n == nil {
		return ErrExternalUnknown
	}
	n.drained = drained
	return nil
}

// list returns every external node, without API keys. inflight and pins are
// by node id.
func (e *externals) list(inflight, pins map[string]int) []External {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []External{}
	for _, n := range e.sorted() {
		id := ExternalPrefix + n.cfg.Name
		x := External{Name: n.cfg.Name, NodeID: id, URL: n.cfg.URL, HasAPIKey: n.cfg.APIKey != "",
			Models: slices.Clone(n.cfg.Models), DiscoveredModels: slices.Clone(n.discovered),
			MaxConcurrency: n.cfg.MaxConcurrency, CtxSize: n.cfg.CtxSize, SpeedTPS: n.cfg.SpeedTPS,
			State: n.state, LastCheck: n.lastCheck, LastError: n.lastErr, Measured: map[string]ExternalSpeed{},
			Drained: n.drained, Inflight: inflight[id], PinnedSessions: pins[id]}
		if x.Models == nil {
			x.Models = []string{}
		}
		if x.DiscoveredModels == nil {
			x.DiscoveredModels = []string{}
		}
		for m, v := range n.cfg.Measured {
			x.Measured[m] = v
		}
		out = append(out, x)
	}
	return out
}

// get returns one external node, without its API key.
func (e *externals) get(name string, inflight, pins map[string]int) (External, bool) {
	for _, x := range e.list(inflight, pins) {
		if x.Name == name {
			return x, true
		}
	}
	return External{}, false
}

// speed is the routing speed of one model: the operator's hint, else the
// self-test generation tok/s, else 0 (unknown). Call with mu held.
func (n *extNode) speed(model string) float64 {
	if n.cfg.SpeedTPS > 0 {
		return n.cfg.SpeedTPS
	}
	return n.cfg.Measured[model].GenTPS
}

// backends returns one backend per model of every ACTIVE external node.
func (e *externals) backends() []gateway.Backend {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []gateway.Backend
	for _, n := range e.sorted() {
		if n.state != ExternalActive {
			continue
		}
		for _, m := range n.discovered {
			out = append(out, gateway.Backend{NodeID: ExternalPrefix + n.cfg.Name, Alias: n.cfg.Name, Model: m, URL: n.cfg.URL,
				Speed: n.speed(m), Drained: n.drained, CtxSize: n.cfg.CtxSize, External: true, APIKey: n.cfg.APIKey,
				MaxConcurrency: n.cfg.MaxConcurrency})
		}
	}
	return out
}

// filterModels keeps the discovered models the allowlist permits, in
// allowlist order; an empty allowlist keeps all, in the server's order.
func filterModels(discovered, allow []string) []string {
	if len(allow) == 0 {
		return slices.Clone(discovered)
	}
	var out []string
	for _, m := range allow {
		if slices.Contains(discovered, m) {
			out = append(out, m)
		}
	}
	return out
}

// run polls every external node every ExternalPollInterval, and right after
// a registration, then self-tests models that have no measurement yet.
func (e *externals) run(ctx context.Context) {
	t := time.NewTicker(ExternalPollInterval)
	defer t.Stop()
	for {
		e.pollAll(ctx)
		for _, name := range e.names() {
			go e.selfTest(ctx, name, false)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.kick:
		}
	}
}

func (e *externals) names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, n := range e.sorted() {
		out = append(out, n.cfg.Name)
	}
	return out
}

// pollAll checks every external node in parallel.
func (e *externals) pollAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, name := range e.names() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.poll(ctx, name)
		}()
	}
	wg.Wait()
}

// poll checks one external node: GET {url}/v1/models.
func (e *externals) poll(ctx context.Context, name string) {
	e.mu.Lock()
	n := e.byName[name]
	if n == nil {
		e.mu.Unlock()
		return
	}
	cfg := n.cfg
	e.mu.Unlock()

	ids, err := e.fetchModels(ctx, cfg)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.byName[name] != n { // removed or replaced meanwhile
		return
	}
	n.lastCheck = time.Now()
	if err != nil {
		n.fails++
		n.lastErr = err.Error()
		if n.fails >= externalFailsOffline || n.state != ExternalActive {
			e.setState(n, ExternalOffline, err.Error())
		}
		return
	}
	n.fails, n.lastErr = 0, ""
	disc := filterModels(ids, cfg.Models)
	if !slices.Equal(disc, n.discovered) {
		e.log.Info("external models changed", "name", name, "models", disc, "from", n.discovered)
		n.discovered = disc
	}
	e.setState(n, ExternalActive, "models listed")
}

// setState must be called with mu held.
func (e *externals) setState(n *extNode, to, reason string) {
	from := n.state
	if from == to {
		return
	}
	n.state = to
	e.log.Info("external state transition", "name", n.cfg.Name, "node_id", ExternalPrefix+n.cfg.Name, "from", from, "to", to, "reason", reason)
	e.transitions.WithLabelValues(from, to).Inc()
}

func (e *externals) fetchModels(ctx context.Context, cfg ExternalConfig) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, externalPollTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/models: HTTP %d: %s", resp.StatusCode, truncate(string(bytes.TrimSpace(body)), 200))
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("GET /v1/models: not an OpenAI model list: %v", err)
	}
	var ids []string
	for _, m := range list.Data {
		if m.ID != "" && !slices.Contains(ids, m.ID) {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// selfTest measures each discovered model's speed: every model with all
// set, else only models without a measurement. Only one self-test runs per
// node at a time; it returns false if one is already running.
func (e *externals) selfTest(ctx context.Context, name string, all bool) (bool, error) {
	e.mu.Lock()
	n := e.byName[name]
	switch {
	case n == nil:
		e.mu.Unlock()
		return false, ErrExternalUnknown
	case n.testing:
		e.mu.Unlock()
		return false, nil
	}
	cfg := n.cfg
	var todo []string
	for _, m := range n.discovered {
		if _, done := cfg.Measured[m]; all || !done {
			todo = append(todo, m)
		}
	}
	if n.state != ExternalActive || len(todo) == 0 {
		e.mu.Unlock()
		return true, nil
	}
	n.testing = true
	e.mu.Unlock()

	results := map[string]ExternalSpeed{}
	for _, m := range todo {
		sp := e.measure(ctx, cfg, m)
		results[m] = sp
		e.log.Info("external self-test", "name", name, "model", m, "gen_tps", sp.GenTPS, "prompt_tps", sp.PromptTPS, "err", sp.Error)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	n.testing = false
	if e.byName[name] != n {
		return true, nil
	}
	if n.cfg.Measured == nil {
		n.cfg.Measured = map[string]ExternalSpeed{}
	}
	for m, sp := range results {
		n.cfg.Measured[m] = sp
	}
	return true, e.save()
}

// measure runs one chat completion with the fixed prompt and max_tokens 32.
// It prefers llama.cpp timings, else completion tokens over wall-clock time
// (which then includes prompt processing).
func (e *externals) measure(ctx context.Context, cfg ExternalConfig, model string) ExternalSpeed {
	sp := ExternalSpeed{At: time.Now()}
	fail := func(err error) ExternalSpeed {
		sp.Error = err.Error()
		return sp
	}
	body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 32, "temperature": 0, "stream": false,
		"messages": []map[string]string{{"role": "user", "content": externalSelfTestPrompt}}})
	ctx, cancel := context.WithTimeout(ctx, externalSelfTestTime)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fail(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	start := time.Now()
	resp, err := e.client.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	elapsed := time.Since(start).Seconds()
	if err != nil {
		return fail(err)
	}
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(bytes.TrimSpace(raw)), 200)))
	}
	var v struct {
		Usage *struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Timings *struct {
			PromptPS float64 `json:"prompt_per_second"`
			PredPS   float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return fail(fmt.Errorf("not a chat completion: %v", err))
	}
	if v.Timings != nil && v.Timings.PredPS > 0 {
		sp.GenTPS, sp.PromptTPS = v.Timings.PredPS, v.Timings.PromptPS
		return sp
	}
	if v.Usage == nil || v.Usage.CompletionTokens == 0 || elapsed <= 0 {
		return fail(errors.New("response has no usage or timings"))
	}
	sp.GenTPS = float64(v.Usage.CompletionTokens) / elapsed
	return sp
}

// externalCollector exports external nodes' state at scrape time.
type externalCollector struct{ e *externals }

var descExternalUp = prometheus.NewDesc("phoneborg_external_up", "1 if the external node is ACTIVE (ADR-016).", []string{"node_id"}, nil)

func (c *externalCollector) Describe(ch chan<- *prometheus.Desc) { ch <- descExternalUp }

func (c *externalCollector) Collect(ch chan<- prometheus.Metric) {
	for _, x := range c.e.list(nil, nil) {
		up := 0.0
		if x.State == ExternalActive {
			up = 1
		}
		ch <- prometheus.MustNewConstMetric(descExternalUp, prometheus.GaugeValue, up, x.NodeID)
	}
}

// externalMembers computes each external node's pool eligibility, one
// member per discovered model. Class filters never match: externals have no
// RAM class.
func externalMembers(p Pool, exts []External) []PoolMember {
	var out []PoolMember
	for _, x := range exts {
		models := x.DiscoveredModels
		if len(models) == 0 {
			models = []string{""}
		}
		for _, model := range models {
			m := PoolMember{NodeID: x.NodeID, Alias: x.Name, Model: model}
			speed := x.SpeedTPS
			if speed == 0 {
				speed = x.Measured[model].GenTPS
			}
			switch {
			case len(p.Nodes) > 0 && !slices.Contains(p.Nodes, x.NodeID) && !slices.Contains(p.Nodes, x.Name):
				m.Reason = ReasonNotMember
			case len(p.Classes) > 0:
				m.Reason = ReasonClass
			case x.Drained:
				m.Reason = ReasonDrained
			case x.State != ExternalActive || model == "":
				m.Reason = ReasonNotReady
			case len(p.Models) > 0 && !slices.Contains(p.Models, model):
				m.Reason = ReasonModel
			case p.MinGenTPS > 0 && speed < p.MinGenTPS:
				m.Reason = ReasonBelowMinTPS
			}
			m.Eligible = m.Reason == ""
			out = append(out, m)
		}
	}
	return out
}

// Admin handlers.

func (s *Server) registerExternalAdmin(add func(pattern, action string, fn http.HandlerFunc)) {
	add("GET /admin/external", "external_list", s.adminExternals)
	add("PUT /admin/external/{name}", "external_set", s.adminSetExternal)
	add("DELETE /admin/external/{name}", "external_delete", s.adminDeleteExternal)
	add("POST /admin/external/{name}/selftest", "external_selftest", s.adminSelfTestExternal)
}

func (s *Server) externalList() []External { return s.ext.list(s.gw.Inflight(), s.affinity.Pins()) }

func (s *Server) adminExternals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Externals{External: s.externalList()})
}

func (s *Server) adminSetExternal(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var spec ExternalSpec
	if !decodeStrict(w, r, &spec) {
		return
	}
	var err error
	code := http.StatusBadRequest
	switch {
	case !validAlias(name):
		err = ErrExternalName
	default:
		if _, taken := s.reg.Lookup(name); taken {
			err, code = ErrExternalTaken, http.StatusConflict
		}
	}
	if err == nil {
		spec, err = normExternalSpec(spec)
	}
	created := false
	if err == nil {
		if created, err = s.ext.put(name, spec); err != nil {
			code, err = http.StatusInternalServerError, fmt.Errorf("cannot store external nodes: %w", err)
		}
	}
	s.audit(r, "external_set", err, "name", name, "url", spec.URL, "has_api_key", spec.APIKey != nil && *spec.APIKey != "")
	if err != nil {
		httpError(w, code, err.Error())
		return
	}
	s.ext.poll(r.Context(), name) // show the state right away
	x, _ := s.ext.get(name, s.gw.Inflight(), s.affinity.Pins())
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, x)
}

func (s *Server) adminDeleteExternal(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.ext.remove(name)
	if err == nil {
		s.affinity.Unpin(ExternalPrefix + name)
	}
	s.audit(r, "external_delete", err, "name", name)
	switch {
	case errors.Is(err, ErrExternalUnknown):
		httpError(w, http.StatusNotFound, "unknown external node "+name)
	case err != nil:
		httpError(w, http.StatusInternalServerError, "cannot store external nodes: "+err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// adminSelfTestExternal re-measures every discovered model of an external
// node and returns the node. It waits for the self-tests.
func (s *Server) adminSelfTestExternal(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.ext.has(name) {
		s.ext.poll(r.Context(), name)
	}
	started, err := s.ext.selfTest(r.Context(), name, true)
	s.audit(r, "external_selftest", err, "name", name)
	switch {
	case errors.Is(err, ErrExternalUnknown):
		httpError(w, http.StatusNotFound, "unknown external node "+name)
		return
	case err != nil:
		httpError(w, http.StatusInternalServerError, "cannot store external nodes: "+err.Error())
		return
	case !started:
		httpError(w, http.StatusConflict, "a self-test of "+name+" is already running")
		return
	}
	x, _ := s.ext.get(name, s.gw.Inflight(), s.affinity.Pins())
	writeJSON(w, http.StatusOK, x)
}
