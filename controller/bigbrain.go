package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/proto"
)

// Big Brain pools (ADR-034): one model too big for any single phone, split
// across the RAM of several. The members run ggml-rpc-server (desired mode
// proto.ModeRPC) on their serving port; the controller runs the head, a
// llama-server built with RPC on this host, which keeps no weights itself
// and drives the members over the serving ports' adb forwards. The head is
// a gateway backend reachable only through pool/<name>.

// RoutingBigBrain makes a pool a Big Brain pool.
const RoutingBigBrain = "bigbrain"

// Big Brain states.
const (
	BigBrainPreparing = "preparing" // waiting for every member to run ggml-rpc-server
	BigBrainLoading   = "loading"   // the head is pushing the weights to the members
	BigBrainReady     = "ready"
	BigBrainError     = "error"
	BigBrainStopped   = "stopped" // the pool is disabled; its phones serve their own models
)

// Speculative decoding modes of a Big Brain head (ADR-036).
const (
	SpecOff   = ""
	SpecDraft = "draft" // a smaller catalog model drafts on the host CPU
	SpecNgram = "ngram" // drafts from the context, no model
)

const bigBrainDefaultSpecMax = 4

// ReasonNotRPC: a Big Brain member that does not run ggml-rpc-server (yet).
const ReasonNotRPC = "not in rpc mode"

// ReasonNotPrima: a Big Brain member that does not run the prima.cpp worker
// of the current ring (yet).
const ReasonNotPrima = "not in prima mode"

const (
	bigBrainTick = 3 * time.Second
	// bigBrainReserve is the RAM left free on every member after its share
	// of the weights: Android, the agent and the RPC compute buffers.
	bigBrainReserve = 900 << 20
	// bigBrainKVMargin covers the KV cache and compute buffers on top of
	// the weights.
	bigBrainKVMargin   = 1.05
	bigBrainNodePrefix = "bb:"
	bigBrainMinBackoff = 30 * time.Second
	bigBrainMaxBackoff = 10 * time.Minute
	bigBrainDefaultCtx = 4096
	// bigBrainSettle is how long after a head stops its members' reports are
	// not trusted for the fit check: the phones free the old weights, and
	// their next heartbeats show it.
	bigBrainSettle = 15 * time.Second
)

// BigBrainOptions configures Big Brain heads.
type BigBrainOptions struct {
	// HeadBin is llama-server built with -DGGML_RPC=ON for this host
	// (make llama-head); "" = Big Brain pools cannot start.
	HeadBin string
	// PrimaHeadBin is llama-server built from prima.cpp for this host (make
	// prima-head); "" = prima.cpp pools cannot start (ADR-037).
	PrimaHeadBin string
	BasePort     int    // first head port on 127.0.0.1; 0 = 18200
	LogDir       string // head logs, one file per pool; "" = discarded
	// Host reaches the members' advertised ports (the adb forwards); it is
	// the gateway's BackendHost.
	Host string
}

// BigBrainStatus is a Big Brain pool's computed state in /admin/pools.
type BigBrainStatus struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// NeedBytes is the model plus margin; HaveBytes the members' free RAM
	// after the per-member reserve, as last reported.
	NeedBytes int64 `json:"need_bytes"`
	HaveBytes int64 `json:"have_bytes"`
	// TensorSplit is each member's share of the weights (MiB of usable RAM),
	// in Nodes order, as passed to the running head (-ts).
	TensorSplit []int `json:"tensor_split,omitempty"`
	// Backend is BackendRPC or BackendPrima; LayerWindows are the layers
	// per rank of the running prima ring, rank 0 (the host) first.
	Backend       string    `json:"backend,omitempty"`
	LayerWindows  []int     `json:"layer_windows,omitempty"`
	Port          int       `json:"port,omitempty"`
	LoadStartedAt time.Time `json:"load_started_at,omitzero"`
	LoadSeconds   float64   `json:"load_seconds,omitempty"`
	GenTPS        float64   `json:"gen_tps,omitempty"`
	PromptTPS     float64   `json:"prompt_tps,omitempty"`
	Restarts      int       `json:"restarts,omitempty"`
	// Spec is the speculative decoding the running head uses ("draft:<id>",
	// "ngram"); SpecNote says why the pool's setting is not in effect.
	Spec     string `json:"spec,omitempty"`
	SpecNote string `json:"spec_note,omitempty"`
}

// headSpec is the speculative decoding a head runs with: the pool's setting
// as far as it can be honoured now.
type headSpec struct {
	mode  string // SpecOff, SpecDraft or SpecNgram
	draft string // catalog id, SpecDraft
	path  string // the draft model's file, SpecDraft
	max   int
	note  string // why the pool's setting is not in effect
}

func (s headSpec) label() string {
	if s.mode == SpecDraft {
		return SpecDraft + ":" + s.draft
	}
	return s.mode
}

// bigBrainMember is a resolved member of a Big Brain pool.
type bigBrainMember struct {
	name     string // as written in the pool
	id       string // node id; "" = unknown node
	rpcAddr  string // host:port of its ggml-rpc-server; "" = not in RPC mode
	state    string // its runtime state, for reasons
	freeRAM  uint64
	excluded string // drained or unknown; "" = usable
	// stale: its heartbeats are late (SUSPECT/OFFLINE), so rpcAddr is its
	// last report. Loading saturates the USB link and delays heartbeats, so
	// a running head keeps a stale member; its RPC connection fails if the
	// phone is really gone.
	stale string
}

// head is one Big Brain pool's head process and state.
type head struct {
	key     string // configuration it runs: restarted when this changes
	cancel  context.CancelFunc
	done    chan struct{}
	exited  bool
	logTail string
	status  BigBrainStatus
	retryAt time.Time // no restart before this after the head exited
	backoff time.Duration
	stopped time.Time // when a running head was last stopped
	// prima.cpp (ADR-037): the ring of the running head, and the epoch the
	// workers run with; it is bumped whenever the ring is (re)started or
	// breaks, because the workers must restart together.
	ring  *primaRing
	epoch int64
}

type bigBrains struct {
	opts BigBrainOptions
	log  *slog.Logger
	http *http.Client

	mu    sync.Mutex
	heads map[string]*head // by pool name
}

func newBigBrains(opts BigBrainOptions, log *slog.Logger) *bigBrains {
	if opts.BasePort == 0 {
		opts.BasePort = 18200
	}
	return &bigBrains{opts: opts, log: log.With("component", "bigbrain"), http: &http.Client{Timeout: 2 * time.Second},
		heads: map[string]*head{}}
}

func bigBrainNodeID(pool string) string { return bigBrainNodePrefix + pool }

// RunBigBrain supervises the Big Brain heads until ctx ends.
func (s *Server) RunBigBrain(ctx context.Context) {
	t := time.NewTicker(bigBrainTick)
	defer t.Stop()
	for {
		s.bigBrainTick(ctx)
		select {
		case <-ctx.Done():
			s.bb.stopAll()
			return
		case <-t.C:
		}
	}
}

// bigBrainPools returns the enabled Big Brain pools.
func (s *Server) bigBrainPools() []Pool {
	var out []Pool
	for _, p := range s.pools.list() {
		if p.Routing == RoutingBigBrain && !p.Disabled {
			out = append(out, p)
		}
	}
	return out
}

// bigBrainModel returns the model nodeID lends its RAM to: it is a member of
// an enabled Big Brain pool whose model is ready in the catalog, and the
// controller can run heads.
func (s *Server) bigBrainModel(nodeID string) (string, bool) {
	p, _, ok := s.bigBrainPoolOf(nodeID)
	if !ok || s.bb.headBin(p) == "" {
		return "", false
	}
	_, _, ready := s.catalog.File(p.Model)
	return p.Model, ready
}

// bigBrainPoolOf returns the enabled Big Brain pool nodeID is a member of,
// and its index among the pool's nodes.
func (s *Server) bigBrainPoolOf(nodeID string) (Pool, int, bool) {
	for _, p := range s.bigBrainPools() {
		for i, n := range p.Nodes {
			if id, ok := s.reg.Lookup(n); (ok && id == nodeID) || n == nodeID {
				return p, i, true
			}
		}
	}
	return Pool{}, 0, false
}

// bigBrainMembers resolves p's nodes against the registry.
func (s *Server) bigBrainMembers(p Pool) []bigBrainMember {
	nodes, drained := s.reg.View()
	byID := map[string]proto.Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	out := make([]bigBrainMember, 0, len(p.Nodes))
	for _, name := range p.Nodes {
		m := bigBrainMember{name: name}
		id, ok := s.reg.Lookup(name)
		if !ok {
			id = name
		}
		n, ok := byID[id]
		if !ok {
			m.excluded = "not connected"
			out = append(out, m)
			continue
		}
		m.id = id
		var rt *proto.RuntimeStatus
		if hb := n.LastHeartbeat; hb != nil {
			m.freeRAM, rt = hb.RAMAvailBytes, hb.Runtime
		}
		m.state = runtimeState(rt)
		if n.State != proto.StateActive {
			m.stale = strings.ToLower(string(n.State))
		}
		switch {
		case drained[id]:
			m.excluded = ReasonDrained
		case p.Backend == BackendPrima:
			switch {
			case rt != nil && rt.State == proto.ModePrima && rt.PrimaEpoch != s.bb.epochOf(p.Name):
				m.state = "restarting" // still on the previous ring
			case rt != nil && rt.State == proto.ModePrima && rt.AdvertisePort != 0:
				m.rpcAddr = s.ringAddr(rt)
			}
		case rt != nil && rt.State == proto.ModeRPC && rt.AdvertisePort != 0:
			m.rpcAddr = s.ringAddr(rt)
		}
		out = append(out, m)
	}
	return out
}

// ringAddr is host:port of a member's serving port, reached over its adb
// forward.
func (s *Server) ringAddr(rt *proto.RuntimeStatus) string {
	host := rt.AdvertiseHost
	if host == "" {
		host = s.bb.opts.Host
	}
	return fmt.Sprintf("%s:%d", host, rt.AdvertisePort)
}

// bigBrainSplit returns the members' usable RAM (free minus the reserve) in
// total, and per member in MiB as -ts weights.
func bigBrainSplit(members []bigBrainMember) (have int64, split []int) {
	for _, m := range members {
		u := max(int64(m.freeRAM)-bigBrainReserve, 0)
		have += u
		split = append(split, int(u>>20))
	}
	return have, split
}

// bigBrainTick reconciles the heads with the enabled Big Brain pools.
func (s *Server) bigBrainTick(ctx context.Context) {
	wanted := map[string]bool{}
	for _, p := range s.bigBrainPools() {
		wanted[p.Name] = true
		s.reconcileBigBrain(ctx, p)
	}
	b := s.bb
	b.mu.Lock()
	var stop []string
	for name := range b.heads {
		if !wanted[name] {
			stop = append(stop, name)
		}
	}
	b.mu.Unlock()
	for _, name := range stop {
		b.stop(name, true)
	}
}

func (s *Server) reconcileBigBrain(ctx context.Context, p Pool) {
	b := s.bb
	path, m, ok := s.catalog.File(p.Model)
	members := s.bigBrainMembers(p)
	ctxSize := p.CtxSize
	if ctxSize == 0 {
		ctxSize = bigBrainDefaultCtx
	}
	need := int64(float64(m.SizeBytes) * bigBrainKVMargin)

	b.mu.Lock()
	h := b.heads[p.Name]
	if h == nil {
		h = &head{}
		b.heads[p.Name] = h
	}
	running := h.cancel != nil && !h.exited
	b.mu.Unlock()

	var waiting []string
	addrs := make([]string, 0, len(members))
	for _, mb := range members {
		switch {
		case mb.excluded != "":
			waiting = append(waiting, mb.name+" is "+mb.excluded)
		case mb.rpcAddr == "":
			waiting = append(waiting, mb.name+" is "+mb.state)
		case mb.stale != "" && !running:
			waiting = append(waiting, mb.name+" is "+mb.stale)
		}
		addrs = append(addrs, mb.rpcAddr)
	}
	spec := s.headSpec(p)
	key := fmt.Sprintf("%s|%s|%d|%s|%s|%s|%d|%s", p.Model, path, ctxSize, strings.Join(addrs, ","), spec.mode, spec.path, spec.max, p.Backend)

	b.mu.Lock()
	h.status.NeedBytes = need
	if !running {
		h.status.HaveBytes, _ = bigBrainSplit(members)
	}
	b.mu.Unlock()

	switch {
	case b.opts.HeadBin == "" && p.Backend != BackendPrima:
		b.stop(p.Name, false)
		b.setState(p.Name, BigBrainError, "the controller runs without -bigbrain-head (build it with make llama-head)")
		return
	case b.opts.PrimaHeadBin == "" && p.Backend == BackendPrima:
		b.stop(p.Name, false)
		b.setState(p.Name, BigBrainError, "the controller runs without -bigbrain-prima-head (build it with make prima-head)")
		return
	case !ok:
		b.stop(p.Name, false)
		b.setState(p.Name, BigBrainError, "model "+p.Model+" is not ready in the catalog")
		return
	case p.Backend == BackendPrima && checkPrimaModel(m) != nil:
		b.stop(p.Name, false)
		b.setState(p.Name, BigBrainError, checkPrimaModel(m).Error())
		return
	case len(waiting) > 0:
		if running {
			b.log.Warn("Big Brain member left its mode, stopping the head", "pool", p.Name, "waiting", strings.Join(waiting, ", "))
		}
		b.stop(p.Name, false)
		b.setState(p.Name, BigBrainPreparing, "waiting: "+strings.Join(waiting, ", "))
		return
	}

	b.mu.Lock()
	if running && h.key != key {
		b.mu.Unlock()
		b.log.Info("Big Brain configuration changed, restarting the head", "pool", p.Name)
		b.stop(p.Name, false)
		b.mu.Lock()
		running = false
	}
	if running {
		b.mu.Unlock()
		s.pollBigBrain(ctx, p.Name)
		return
	}
	if h.exited && time.Now().Before(h.retryAt) {
		h.status.State = BigBrainError
		h.status.Reason = fmt.Sprintf("head exited, retrying at %s: %s", h.retryAt.Format(time.TimeOnly), h.logTail)
		b.mu.Unlock()
		return
	}
	settled := time.Since(h.stopped) >= bigBrainSettle
	b.mu.Unlock()

	if !settled {
		b.setState(p.Name, BigBrainPreparing, "waiting for the phones to free the previous load")
		return
	}
	have, split := bigBrainSplit(members)
	if have < need {
		b.setState(p.Name, BigBrainError, fmt.Sprintf("the model needs %.1f GB, the members have %.1f GB free: add phones or pick a smaller model",
			float64(need)/1e9, float64(have)/1e9))
		return
	}
	var ring *primaRing
	var windows []int
	if p.Backend == BackendPrima {
		weights := make([]int64, len(split))
		ids := make([]string, len(members))
		for i, v := range split {
			weights[i], ids[i] = int64(v), members[i].id
		}
		var err error
		if windows, err = primaWindows(m.Layers, weights); err == nil {
			err = primaFit(members, windows, m.Layers, m.SizeBytes)
		}
		if err != nil {
			b.setState(p.Name, BigBrainError, err.Error())
			return
		}
		ring = &primaRing{ids: ids, addrs: addrs}
	}
	b.start(ctx, p, path, ctxSize, addrs, split, spec, key, ring, windows)
}

// headSpec resolves p's speculative decoding. A draft model that is not
// ready (yet) leaves the head running without it rather than failing the pool.
func (s *Server) headSpec(p Pool) headSpec {
	spec := headSpec{mode: p.Spec, max: p.SpecMax}
	if spec.max == 0 {
		spec.max = bigBrainDefaultSpecMax
	}
	if p.Spec == SpecDraft {
		path, _, ok := s.catalog.File(p.Draft)
		if !ok {
			return headSpec{note: "draft model " + p.Draft + " is not ready in the catalog: running without it"}
		}
		spec.draft, spec.path = p.Draft, path
	}
	return spec
}

// checkDraft verifies p's draft model: ready, smaller than the pool's model
// and sharing its tokenizer.
func (s *Server) checkDraft(p Pool) error {
	dpath, dm, ok := s.catalog.File(p.Draft)
	if !ok {
		return fmt.Errorf("draft model %q is not ready in the catalog", p.Draft)
	}
	path, m, ok := s.catalog.File(p.Model)
	if !ok {
		return nil // the head waits for the model; compare when it is ready
	}
	if dm.SizeBytes >= m.SizeBytes {
		return fmt.Errorf("draft model %q must be smaller than %q", p.Draft, p.Model)
	}
	mt, err := models.TokenizerOf(path)
	if err != nil {
		return err
	}
	dt, err := models.TokenizerOf(dpath)
	if err != nil {
		return err
	}
	if err := models.DraftCompatible(mt, dt); err != nil {
		return fmt.Errorf("draft model %q cannot draft for %q: %w", p.Draft, p.Model, err)
	}
	return nil
}

// headArgs is the head's llama-server command line: every layer on the
// members (-ngl 99) in proportion to their usable RAM (-ts), one slot, a
// q8_0 KV cache with flash attention. The head computes nothing itself.
// Speculative decoding (ADR-036) adds the draft model on the host CPU (-devd
// none, -ngld 0: the head's only devices are the members) or n-gram drafting.
func headArgs(model, path string, ctxSize, port int, addrs []string, split []int, spec headSpec) []string {
	ts := make([]string, len(split))
	for i, v := range split {
		ts[i] = strconv.Itoa(v)
	}
	args := []string{"-m", path, "-a", model, "--rpc", strings.Join(addrs, ","), "-ngl", "99",
		"-ts", strings.Join(ts, ","), "-c", strconv.Itoa(ctxSize), "-np", "1",
		"-fa", "on", "-ctk", "q8_0", "-ctv", "q8_0", "-t", "2",
		"--host", "127.0.0.1", "--port", strconv.Itoa(port)}
	switch spec.mode {
	case SpecDraft:
		args = append(args, "-md", spec.path, "--spec-type", "draft-simple", "--spec-draft-n-max", strconv.Itoa(spec.max),
			"-devd", "none", "-ngld", "0", "-td", "2")
	case SpecNgram:
		args = append(args, "--spec-type", "ngram-mod", "--spec-ngram-mod-n-max", strconv.Itoa(spec.max))
	}
	return args
}

func (b *bigBrains) start(ctx context.Context, p Pool, path string, ctxSize int, addrs []string, split []int, spec headSpec, key string, ring *primaRing, windows []int) {
	b.mu.Lock()
	h := b.heads[p.Name]
	if h.cancel != nil {
		h.cancel() // the previous head exited; release its context
	}
	bin, args, span := b.opts.HeadBin, []string(nil), 1
	if ring != nil {
		bin, span = b.opts.PrimaHeadBin, primaPortSpan
	}
	port := b.freePortLocked(p.Name, span)
	if ring != nil {
		*ring = *newPrimaRing(ring.ids, ring.addrs, port)
		if err := ring.startRelay(); err != nil {
			h.status.State, h.status.Reason = BigBrainError, "cannot start the ring relay: "+err.Error()
			h.exited, h.logTail, h.retryAt = true, err.Error(), time.Now().Add(bigBrainMinBackoff)
			b.mu.Unlock()
			return
		}
		args = primaHeadArgs(p.Model, path, ctxSize, ring, windows)
	} else {
		args = headArgs(p.Model, path, ctxSize, port, addrs, split, spec)
	}
	var logf io.WriteCloser = nopCloser{io.Discard}
	logPath := ""
	if b.opts.LogDir != "" {
		if err := os.MkdirAll(b.opts.LogDir, 0o700); err == nil {
			if f, err := os.OpenFile(filepath.Join(b.opts.LogDir, p.Name+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
				logf, logPath = f, f.Name()
			}
		}
	}
	hctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(hctx, bin, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		logf.Close()
		if ring != nil {
			ring.Close()
		}
		h.status.State, h.status.Reason = BigBrainError, "cannot start the head: "+err.Error()
		h.exited, h.logTail, h.retryAt = true, err.Error(), time.Now().Add(bigBrainMinBackoff)
		b.mu.Unlock()
		return
	}
	done := make(chan struct{})
	h.key, h.cancel, h.done, h.exited, h.logTail, h.ring = key, cancel, done, false, "", ring
	h.status = BigBrainStatus{State: BigBrainLoading, Reason: fmt.Sprintf("loading %s onto %d phones", p.Model, len(addrs)),
		NeedBytes: h.status.NeedBytes, HaveBytes: h.status.HaveBytes, TensorSplit: split, Port: port,
		LayerWindows: windows, LoadStartedAt: time.Now(), Restarts: h.status.Restarts, Spec: spec.label(), SpecNote: spec.note}
	if ring != nil {
		h.status.TensorSplit = nil
	}
	epoch := h.epoch
	b.mu.Unlock()
	b.log.Info("Big Brain head started", "pool", p.Name, "model", p.Model, "pid", cmd.Process.Pid, "port", port,
		"backend", bigBrainBackend(p), "members", strings.Join(addrs, ","), "tensor_split", split, "layer_windows", windows,
		"epoch", epoch, "spec", spec.label(), "log", logPath)
	go func() {
		err := cmd.Wait()
		logf.Close()
		tail := ""
		if logPath != "" {
			tail = logTail(logPath, 3)
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		close(done)
		if b.heads[p.Name] != h || h.done != done || hctx.Err() != nil {
			return // stopped on purpose
		}
		h.exited, h.logTail = true, tail
		if h.ring != nil {
			h.ring.Close()
			h.epoch = nextEpoch(h.epoch) // the workers hang on a dead ring: restart them all
		}
		h.backoff = min(max(h.backoff*2, bigBrainMinBackoff), bigBrainMaxBackoff)
		h.retryAt = time.Now().Add(h.backoff)
		h.status.Restarts++
		h.status.State, h.status.Reason = BigBrainError, "head exited: "+tail
		b.log.Error("Big Brain head exited", "pool", p.Name, "err", err, "retry_in", h.backoff.String(), "log_tail", tail)
	}()
}

// pollBigBrain moves a loading head to ready once /health answers 200, then
// measures its speed once.
func (s *Server) pollBigBrain(ctx context.Context, pool string) {
	b := s.bb
	b.mu.Lock()
	h := b.heads[pool]
	port, state := h.status.Port, h.status.State
	b.mu.Unlock()
	if state != BigBrainLoading {
		return
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
	resp, err := b.http.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	b.mu.Lock()
	h.status.State, h.status.Reason = BigBrainReady, ""
	h.status.LoadSeconds = time.Since(h.status.LoadStartedAt).Seconds()
	h.backoff = 0
	load := h.status.LoadSeconds
	b.mu.Unlock()
	b.log.Info("Big Brain head ready", "pool", pool, "load_seconds", load)
	model := ""
	if p, ok := s.pools.get(pool); ok {
		model = p.Model
	}
	go b.selfTest(ctx, pool, port, model)
}

// selfTest measures the head's speed with a short completion, like the
// agents' self-test (ADR-010).
func (b *bigBrains) selfTest(ctx context.Context, pool string, port int, model string) {
	body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 16, "temperature": 0, "cache_prompt": false,
		"messages": []map[string]string{{"role": "user", "content": "Count from one to twenty in words."}}})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		b.log.Warn("Big Brain self-test failed", "pool", pool, "err", err)
		return
	}
	defer resp.Body.Close()
	var v struct {
		Timings struct {
			PromptPerSecond    float64 `json:"prompt_per_second"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		b.log.Warn("Big Brain self-test failed", "pool", pool, "status", resp.StatusCode, "err", err)
		return
	}
	b.mu.Lock()
	if h := b.heads[pool]; h != nil && h.status.Port == port {
		h.status.GenTPS, h.status.PromptTPS = v.Timings.PredictedPerSecond, v.Timings.PromptPerSecond
	}
	b.mu.Unlock()
	b.log.Info("Big Brain self-test", "pool", pool, "gen_tps", v.Timings.PredictedPerSecond, "prompt_tps", v.Timings.PromptPerSecond)
}

// freePortLocked returns the lowest first port of span consecutive head
// ports that no other running head uses.
func (b *bigBrains) freePortLocked(pool string, span int) int {
	used := map[int]bool{}
	for name, h := range b.heads {
		if name != pool && h.cancel != nil && !h.exited {
			n := 1
			if h.ring != nil {
				n = primaPortSpan
			}
			for i := range n {
				used[h.status.Port+i] = true
			}
		}
	}
	port := b.opts.BasePort
	for i := 0; i < span; i++ {
		if used[port+i] {
			port, i = port+i+1, -1
		}
	}
	return port
}

func (b *bigBrains) setState(pool, state, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if h := b.heads[pool]; h != nil {
		h.status.State, h.status.Reason = state, reason
	}
}

// stop ends pool's head and waits for it; forget also drops its status.
func (b *bigBrains) stop(pool string, forget bool) {
	b.mu.Lock()
	h := b.heads[pool]
	if h == nil {
		b.mu.Unlock()
		return
	}
	cancel, done, ring := h.cancel, h.done, h.ring
	if ring != nil && cancel != nil && !h.exited {
		h.epoch = nextEpoch(h.epoch) // the workers must restart with the next ring
	}
	h.cancel, h.done, h.key, h.ring = nil, nil, "", nil
	if cancel != nil {
		h.exited, h.stopped = false, time.Now()
	}
	h.status.LayerWindows = nil
	h.status.TensorSplit, h.status.Port, h.status.GenTPS, h.status.PromptTPS = nil, 0, 0, 0
	h.status.LoadStartedAt, h.status.LoadSeconds = time.Time{}, 0
	if forget {
		delete(b.heads, pool)
	}
	b.mu.Unlock()
	if ring != nil {
		ring.Close()
	}
	if cancel != nil {
		cancel()
		<-done
		b.log.Info("Big Brain head stopped", "pool", pool)
	}
}

func (b *bigBrains) stopAll() {
	b.mu.Lock()
	names := make([]string, 0, len(b.heads))
	for n := range b.heads {
		names = append(names, n)
	}
	b.mu.Unlock()
	for _, n := range names {
		b.stop(n, true)
	}
}

// status returns pool's computed state; BigBrainPreparing before the
// supervisor first saw the pool.
func (b *bigBrains) status(pool string) BigBrainStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.heads[pool]
	if h == nil || h.status.State == "" {
		return BigBrainStatus{State: BigBrainPreparing}
	}
	st := h.status
	st.TensorSplit = slices.Clone(st.TensorSplit)
	return st
}

// bigBrainBackends returns the ready heads as gateway backends, reachable
// only through their pool (PoolOnly).
func (s *Server) bigBrainBackends() []gateway.Backend {
	var out []gateway.Backend
	for _, p := range s.bigBrainPools() {
		st := s.bb.status(p.Name)
		if st.State != BigBrainReady {
			continue
		}
		ctxSize := p.CtxSize
		if ctxSize == 0 {
			ctxSize = bigBrainDefaultCtx
		}
		out = append(out, gateway.Backend{NodeID: bigBrainNodeID(p.Name), Alias: bigBrainNodeID(p.Name), Model: p.Model,
			URL: fmt.Sprintf("http://127.0.0.1:%d", st.Port), Speed: st.GenTPS, CtxSize: ctxSize,
			MaxConcurrency: 1, PoolOnly: true})
	}
	return out
}

// bigBrainPoolMembers lists a Big Brain pool's phones, eligible once they
// run ggml-rpc-server.
func (s *Server) bigBrainPoolMembers(p Pool) []PoolMember {
	nodes, _ := s.reg.View()
	alias := map[string]string{}
	for _, n := range nodes {
		alias[n.ID] = n.Alias
	}
	out := []PoolMember{}
	for _, mb := range s.bigBrainMembers(p) {
		m := PoolMember{NodeID: mb.id, Alias: alias[mb.id], Model: p.Model}
		switch {
		case mb.id == "":
			m.NodeID, m.Reason = mb.name, mb.excluded
		case mb.excluded != "":
			m.Reason = mb.excluded
		case mb.rpcAddr == "" && p.Backend == BackendPrima:
			m.Reason = ReasonNotPrima + " (" + mb.state + ")"
		case mb.rpcAddr == "":
			m.Reason = ReasonNotRPC + " (" + mb.state + ")"
		case mb.stale != "":
			m.Reason = mb.stale
		}
		m.Eligible = m.Reason == ""
		out = append(out, m)
	}
	return out
}

// logTail returns the last n non-empty lines of a file, joined by " | ".
func logTail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > 64<<10 {
		data = data[len(data)-64<<10:]
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
