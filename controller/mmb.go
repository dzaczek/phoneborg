package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/proto"
)

// MMB, the multi-model benchmark (ADR-028): load every model that fits on
// the chosen phones, one after the other, and time the same requests on
// each, so models and phones can be compared. A phone under test is drained
// and its model is forced with a benchmark override; afterwards it gets its
// placement and drain state back.

// MMB run and result statuses.
const (
	MMBQueued    = "queued"
	MMBLoading   = "loading"
	MMBRunning   = "running"
	MMBDone      = "done"
	MMBFailed    = "failed"
	MMBSkipped   = "skipped"
	MMBCancelled = "cancelled"
)

// Timing knobs; variables so tests can shorten them.
var (
	mmbLoadTimeout = 30 * time.Minute // download and load of one model
	mmbPollEvery   = 2 * time.Second
	mmbProbeTime   = 20 * time.Minute // one probe request
)

// MMBRequest is the body of POST /admin/mmb.
type MMBRequest struct {
	Nodes    []string `json:"nodes"`    // aliases or node ids
	Pool     string   `json:"pool"`     // or a pool: its member nodes ("pool/<name>" or "<name>")
	Models   []string `json:"models"`   // empty = every ready catalog model that fits each node
	Parallel bool     `json:"parallel"` // benchmark the nodes at the same time instead of one by one
}

// MMBProbe is one timed request; see gateway.ProbeResult.
type MMBProbe = gateway.ProbeResult

// MMBResult is one model on one node.
type MMBResult struct {
	NodeID      string    `json:"node_id"`
	Alias       string    `json:"alias"`
	Device      string    `json:"device"`
	ModelID     string    `json:"model_id"`
	Params      string    `json:"params"`
	SizeBytes   int64     `json:"size_bytes"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	LoadSeconds float64   `json:"load_seconds"` // from the override until the node serves the model
	CtxSize     int       `json:"ctx_size"`
	KVType      string    `json:"kv_type"`
	Cold        *MMBProbe `json:"cold,omitempty"`  // long prompt, empty cache
	Warm        *MMBProbe `json:"warm,omitempty"`  // same long prefix again: the answer after the first prompt
	Short       *MMBProbe `json:"short,omitempty"` // a one-line question
	Started     time.Time `json:"started,omitzero"`
	Finished    time.Time `json:"finished,omitzero"`
}

// MMBRun is one benchmark run.
type MMBRun struct {
	ID        string      `json:"id"`
	Status    string      `json:"status"`
	Parallel  bool        `json:"parallel"`
	Nodes     []string    `json:"nodes"`
	Principal string      `json:"principal"`
	Error     string      `json:"error,omitempty"`
	Results   []MMBResult `json:"results"`
	Created   time.Time   `json:"created"`
	Finished  time.Time   `json:"finished,omitzero"`
}

// MMBRuns is the response of GET /admin/mmb.
type MMBRuns struct {
	Runs []MMBRun `json:"runs"`
}

// MMBOptions configures MMB storage.
type MMBOptions struct {
	Dir string // one <id>.json per run; "" = memory only
}

// mmb runs benchmarks, one run at a time.
type mmb struct {
	s   *Server
	dir string
	log *slog.Logger

	mu     sync.Mutex
	runs   map[string]*MMBRun
	cancel context.CancelFunc // of the running run
	active string

	mProbes *prometheus.CounterVec
}

func newMMB(s *Server, dir string, log *slog.Logger) *mmb {
	b := &mmb{s: s, dir: dir, log: log.With("component", "mmb"), runs: map[string]*MMBRun{},
		mProbes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "phoneborg_mmb_results_total", Help: "Multi-model benchmark results by status (ADR-028)."}, []string{"status"})}
	if dir == "" {
		return b
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		var run MMBRun
		data, err := os.ReadFile(f)
		if err == nil {
			err = json.Unmarshal(data, &run)
		}
		if err != nil { // results are not worth refusing to start for
			b.log.Warn("skipping unreadable benchmark run", "file", f, "err", err)
			continue
		}
		if run.Status == MMBRunning || run.Status == MMBQueued {
			// The controller stopped mid-run; overrides and drains lived in
			// memory, so the nodes are already back to normal.
			run.Status, run.Error = MMBCancelled, "interrupted by a controller restart"
		}
		b.runs[run.ID] = &run
	}
	return b
}

var errMMBBusy = errors.New("a benchmark is already running; cancel it or wait")

// start validates req, plans the run and starts it in the background.
func (b *mmb) start(req MMBRequest, principal string) (MMBRun, error) {
	nodes, err := b.resolveNodes(req)
	if err != nil {
		return MMBRun{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active != "" {
		return MMBRun{}, errMMBBusy
	}
	run := &MMBRun{ID: gateway.NewRequestID(), Status: MMBRunning, Parallel: req.Parallel, Principal: principal,
		Created: time.Now().UTC(), Results: []MMBResult{}}
	for _, n := range nodes {
		run.Nodes = append(run.Nodes, n.ID)
		run.Results = append(run.Results, b.modelsFor(n, req.Models)...)
	}
	if len(run.Results) == 0 {
		return MMBRun{}, errors.New("no model fits the chosen nodes (or none of the requested models is in the catalog)")
	}
	b.runs[run.ID] = run
	b.active = run.ID
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.saveLocked(run)
	go b.execute(ctx, run.ID, nodes)
	return b.copyLocked(run), nil
}

// resolveNodes turns the request's nodes or pool into registered phones.
func (b *mmb) resolveNodes(req MMBRequest) ([]proto.Node, error) {
	s := b.s
	names := slices.Clone(req.Nodes)
	if req.Pool != "" {
		p, ok := s.pools.get(strings.TrimPrefix(req.Pool, poolPrefix))
		if !ok {
			return nil, fmt.Errorf("unknown pool %q", req.Pool)
		}
		for _, m := range s.withMembers(p).Members {
			if m.Reason != ReasonNotMember && m.Reason != ReasonClass && !strings.HasPrefix(m.NodeID, ExternalPrefix) {
				names = append(names, m.NodeID)
			}
		}
	}
	if len(names) == 0 {
		return nil, errors.New("choose nodes or a pool")
	}
	all, _ := s.reg.View()
	var out []proto.Node
	seen := map[string]bool{}
	for _, name := range names {
		id, ok := s.reg.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("unknown node %q (external nodes cannot be benchmarked: they have no placement)", name)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		for _, n := range all {
			if n.ID == id {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// modelsFor lists the results to measure on n: the requested models, or
// every ready catalog model that fits it. A model that does not fit is
// listed as skipped, so the table shows why.
func (b *mmb) modelsFor(n proto.Node, want []string) []MMBResult {
	class := models.ClassOf(n.Inventory.RAMTotalBytes)
	var budget int64
	if hb := n.LastHeartbeat; hb != nil && hb.Runtime != nil {
		budget = hb.Runtime.BudgetBytes
	}
	device := strings.TrimSpace(n.Inventory.Manufacturer + " " + n.Inventory.Model)
	var out []MMBResult
	for _, m := range b.s.catalog.List() {
		if m.Status != models.StatusReady || (len(want) > 0 && !slices.Contains(want, m.ID)) {
			continue
		}
		r := MMBResult{NodeID: n.ID, Alias: n.Alias, Device: device, ModelID: m.ID, Params: m.Params, SizeBytes: m.SizeBytes, Status: MMBQueued}
		resident := m.ResidentBytes
		if resident == 0 {
			resident = m.SizeBytes
		}
		switch {
		case !slices.Contains(m.FitsClasses, class):
			r.Status, r.Error = MMBSkipped, fmt.Sprintf("does not fit a %s phone with a 16k context", class)
		case budget > 0 && resident > budget:
			r.Status, r.Error = MMBSkipped, fmt.Sprintf("needs %.1f GiB, the phone's budget is %.1f GiB", float64(resident)/(1<<30), float64(budget)/(1<<30))
		}
		if r.Status == MMBSkipped && len(want) == 0 {
			continue // only requested models are listed as skipped
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SizeBytes < out[j].SizeBytes })
	return out
}

// execute benchmarks every node of the run, one by one or in parallel.
func (b *mmb) execute(ctx context.Context, id string, nodes []proto.Node) {
	b.mu.Lock()
	parallel := b.runs[id].Parallel
	b.mu.Unlock()
	var wg sync.WaitGroup
	for _, n := range nodes {
		if parallel {
			wg.Add(1)
			go func() { defer wg.Done(); b.benchNode(ctx, id, n) }()
		} else {
			b.benchNode(ctx, id, n)
		}
	}
	wg.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	run := b.runs[id]
	if ctx.Err() != nil {
		run.Status = MMBCancelled
	} else {
		run.Status = MMBDone
	}
	for i := range run.Results {
		if r := &run.Results[i]; r.Status == MMBQueued || r.Status == MMBLoading || r.Status == MMBRunning {
			r.Status = MMBCancelled
		}
	}
	run.Finished = time.Now().UTC()
	b.active, b.cancel = "", nil
	b.saveLocked(run)
	b.log.Info("benchmark finished", "run", id, "status", run.Status)
}

// benchNode measures the run's models on one node, then gives the node its
// placement and drain state back.
func (b *mmb) benchNode(ctx context.Context, id string, n proto.Node) {
	s := b.s
	_, drained := s.reg.View()
	wasDrained := drained[n.ID]
	if !wasDrained {
		_ = s.reg.SetDrained(n.ID, true)
		s.affinity.Unpin(n.ID)
	}
	defer func() {
		s.SetBenchmarkOverride(n.ID, "")
		if !wasDrained {
			_ = s.reg.SetDrained(n.ID, false)
			s.Replan()
		}
	}()
	b.mu.Lock()
	var todo []int
	for i, r := range b.runs[id].Results {
		if r.NodeID == n.ID && r.Status == MMBQueued {
			todo = append(todo, i)
		}
	}
	b.mu.Unlock()
	for _, i := range todo {
		if ctx.Err() != nil {
			return
		}
		b.benchModel(ctx, id, i)
	}
}

// update changes one result under the lock and saves the run.
func (b *mmb) update(id string, i int, f func(r *MMBResult)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	run := b.runs[id]
	f(&run.Results[i])
	b.saveLocked(run)
}

func (b *mmb) benchModel(ctx context.Context, id string, i int) {
	s := b.s
	b.mu.Lock()
	r := b.runs[id].Results[i]
	b.mu.Unlock()
	start := time.Now()
	b.update(id, i, func(r *MMBResult) { r.Status, r.Started = MMBLoading, start.UTC() })
	b.log.Info("benchmark loading", "run", id, "node_id", r.NodeID, "model_id", r.ModelID)
	s.SetBenchmarkOverride(r.NodeID, r.ModelID)

	fail := func(err error) {
		b.mProbes.WithLabelValues(MMBFailed).Inc()
		b.update(id, i, func(r *MMBResult) { r.Status, r.Error, r.Finished = MMBFailed, err.Error(), time.Now().UTC() })
		b.log.Warn("benchmark failed", "run", id, "node_id", r.NodeID, "model_id", r.ModelID, "err", err)
	}
	rt, err := b.waitServing(ctx, r.NodeID, r.ModelID)
	if err != nil {
		if ctx.Err() == nil {
			fail(err)
		}
		return
	}
	b.update(id, i, func(x *MMBResult) {
		x.Status, x.LoadSeconds, x.CtxSize, x.KVType = MMBRunning, time.Since(start).Seconds(), rt.CtxSize, rt.KVType
	})
	p := gateway.Principal{Name: "mmb"}
	probe := func(msgs []map[string]string, maxTokens int, fixed bool) (*MMBProbe, error) {
		pctx, cancel := context.WithTimeout(ctx, mmbProbeTime)
		defer cancel()
		body := map[string]any{"messages": msgs, "max_tokens": maxTokens, "temperature": 0, "cache_prompt": true,
			"chat_template_kwargs": map[string]bool{"enable_thinking": false}}
		if fixed {
			body["ignore_eos"] = true // a fixed answer length makes generation speeds comparable
		}
		res, err := s.gw.Probe(pctx, r.NodeID, body, p)
		return &res, err
	}
	steps := []struct {
		name  string
		msgs  []map[string]string
		max   int
		fixed bool
		set   func(*MMBResult, *MMBProbe)
	}{
		{"cold", mmbLongPrompt("Summarize the text above in three sentences."), 128, true, func(x *MMBResult, v *MMBProbe) { x.Cold = v }},
		{"warm", mmbLongPrompt("List three objects mentioned in the text above."), 128, true, func(x *MMBResult, v *MMBProbe) { x.Warm = v }},
		{"short", []map[string]string{{"role": "user", "content": "What is the capital of France? Answer in one word."}}, 16, false,
			func(x *MMBResult, v *MMBProbe) { x.Short = v }},
	}
	for _, st := range steps {
		v, err := probe(st.msgs, st.max, st.fixed)
		if err != nil {
			if ctx.Err() == nil {
				fail(fmt.Errorf("%s request: %w", st.name, err))
			}
			return
		}
		b.update(id, i, func(x *MMBResult) { st.set(x, v) })
	}
	b.mProbes.WithLabelValues(MMBDone).Inc()
	b.update(id, i, func(x *MMBResult) { x.Status, x.Finished = MMBDone, time.Now().UTC() })
}

// waitServing waits until the node serves model and is ready, or reports
// why it does not.
func (b *mmb) waitServing(ctx context.Context, node, model string) (*proto.RuntimeStatus, error) {
	deadline := time.Now().Add(mmbLoadTimeout)
	for {
		nodes, _ := b.s.reg.View()
		for _, n := range nodes {
			if n.ID != node || n.LastHeartbeat == nil || n.LastHeartbeat.Runtime == nil {
				continue
			}
			rt := n.LastHeartbeat.Runtime
			if currentModel(rt) == model && rt.Ready && runtimeState(rt) == "serving" {
				return rt, nil
			}
			if currentModel(rt) == model && runtimeState(rt) == "error" {
				return nil, fmt.Errorf("the phone could not load the model: %s", rt.Error)
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("not serving after %s", mmbLoadTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(mmbPollEvery):
		}
	}
}

// mmbText is the long, fixed context of the cold and warm requests (about
// 500 tokens), so every model and phone processes the same prompt.
var mmbText = strings.Repeat("The old lighthouse keeper climbed the spiral stairs every evening, carrying a brass lantern, "+
	"a thermos of tea and a notebook in which he wrote down the ships that passed, the color of the sea and the shape of the clouds. "+
	"On the third floor there was a window facing the harbor, where fishermen mended their nets beside a red boat named Morning Star. ", 5)

func mmbLongPrompt(question string) []map[string]string {
	return []map[string]string{{"role": "user", "content": mmbText + "\n\n" + question}}
}

func (b *mmb) cancelRun(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.runs[id]; !ok {
		return ErrUnknownNode
	}
	if b.active == id && b.cancel != nil {
		b.cancel()
	}
	return nil
}

func (b *mmb) list() []MMBRun {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []MMBRun{}
	for _, r := range b.runs {
		out = append(out, b.copyLocked(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func (b *mmb) get(id string) (MMBRun, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.runs[id]
	if !ok {
		return MMBRun{}, false
	}
	return b.copyLocked(r), true
}

func (b *mmb) remove(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.runs[id]; !ok {
		return ErrUnknownNode
	}
	if b.active == id {
		return errors.New("the run is still going; cancel it first")
	}
	delete(b.runs, id)
	if b.dir != "" {
		if err := os.Remove(filepath.Join(b.dir, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (b *mmb) copyLocked(r *MMBRun) MMBRun {
	out := *r
	out.Nodes = slices.Clone(r.Nodes)
	out.Results = slices.Clone(r.Results)
	return out
}

func (b *mmb) saveLocked(r *MMBRun) {
	if b.dir == "" {
		return
	}
	data, err := json.Marshal(r)
	if err == nil {
		if err = os.MkdirAll(b.dir, 0o700); err == nil {
			err = gateway.WriteFileAtomic(filepath.Join(b.dir, r.ID+".json"), data, 0o600)
		}
	}
	if err != nil {
		b.log.Warn("cannot store benchmark run", "run", r.ID, "err", err)
	}
}

// Admin handlers.

func (s *Server) registerMMBAdmin(add func(pattern, action string, fn http.HandlerFunc)) {
	add("GET /admin/mmb", "mmb_list", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, MMBRuns{Runs: s.mmb.list()})
	})
	add("POST /admin/mmb", "mmb_start", s.adminMMBStart)
	add("GET /admin/mmb/{id}", "mmb_get", func(w http.ResponseWriter, r *http.Request) {
		run, ok := s.mmb.get(r.PathValue("id"))
		if !ok {
			httpError(w, http.StatusNotFound, "no such benchmark run")
			return
		}
		writeJSON(w, http.StatusOK, run)
	})
	add("POST /admin/mmb/{id}/cancel", "mmb_cancel", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		err := s.mmb.cancelRun(id)
		s.audit(r, "mmb_cancel", err, "run", id)
		if err != nil {
			httpError(w, http.StatusNotFound, "no such benchmark run")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	add("DELETE /admin/mmb/{id}", "mmb_delete", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		err := s.mmb.remove(id)
		s.audit(r, "mmb_delete", err, "run", id)
		switch {
		case errors.Is(err, ErrUnknownNode):
			httpError(w, http.StatusNotFound, "no such benchmark run")
		case err != nil:
			httpError(w, http.StatusConflict, err.Error())
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
}

func (s *Server) adminMMBStart(w http.ResponseWriter, r *http.Request) {
	var req MMBRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	run, err := s.mmb.start(req, adminPrincipal(r))
	s.audit(r, "mmb_start", err, "nodes", strings.Join(req.Nodes, ","), "pool", req.Pool, "parallel", req.Parallel)
	switch {
	case errors.Is(err, errMMBBusy):
		httpError(w, http.StatusConflict, err.Error())
	case err != nil:
		httpError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusCreated, run)
	}
}
