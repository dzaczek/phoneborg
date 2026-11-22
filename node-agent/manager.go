// Model switching (ADR-012): the Manager reconciles the model the controller
// wants (proto.DesiredRuntime, from a heartbeat response) with the
// llama-server the Runtime supervises, downloading, sizing and loading a new
// model one switch at a time.
package nodeagent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

// loadTimeout bounds how long a newly started llama-server has to answer
// /health before a switch is considered failed.
const loadTimeout = 2 * time.Minute

// Retry backoff for a switch to the same desired state that just failed:
// starts at minBackoff, doubles up to maxBackoff. Without this, a controller
// that keeps assigning a model that does not fit makes the agent retry (and
// re-run sizing) on every heartbeat.
const (
	minBackoff = 30 * time.Second
	maxBackoff = 10 * time.Minute
)

// ManagerConfig holds what stays fixed across model switches: the server
// binary, ports and thread count chosen at startup (ADR-009), plus the
// static model (pcprov's -model) that keeps today's behavior until the
// controller sends a DesiredRuntime.
type ManagerConfig struct {
	ServerBin     string
	Port          int
	AdvertisePort int
	Threads       int
	Variant       string
	ModelsDir     string
	ControllerURL string // prefixed to DesiredRuntime.URL to build the download URL
	MemReserveMB  int

	StaticModelPath string // -model; "" if the node only serves what the controller desires
	StaticCtxSize   int

	// Big Brain (ADR-034): ggml-rpc-server binary ("" = this node cannot
	// join), its thread count, and its tensor cache directory.
	RPCServerBin string
	RPCThreads   int
	RPCCacheDir  string

	// Big Brain on prima.cpp (ADR-037): the worker binary ("" = this node
	// cannot join such a pool) and the node id the controller knows it by.
	PrimaWorkerBin string
	NodeID         string
}

// Manager owns the currently active Runtime and switches it to a new model
// when the controller's desired state changes.
type Manager struct {
	cfg  ManagerConfig
	log  *slog.Logger
	http *http.Client

	mu            sync.Mutex
	runtime       *Runtime // nil when no llama-server is running
	cancel        context.CancelFunc
	done          chan struct{}
	current       string // model_id actually running (matches runtime); "" if none
	residentBytes int64  // resident bytes of the currently running model; 0 = unknown
	lastApplied   *proto.DesiredRuntime

	switching bool
	subState  string // "downloading" | "loading", only meaningful while switching
	target    string // model_id of the in-flight switch, only meaningful while switching
	progress  float64
	lastErr   string
	ramBytes  int64

	// Retry backoff (see minBackoff/maxBackoff) for repeated failures of the
	// same desired state, so the agent does not re-download, re-hash and
	// re-size on every heartbeat while a switch keeps failing.
	lastFailed       *proto.DesiredRuntime // desired state that most recently failed; nil once resolved
	lastFailedBudget int64                 // budgetBytes() at that failure, for the "budget grew" retry override
	backoff          time.Duration         // current backoff before retrying lastFailed
	retryAt          time.Time             // do not retry lastFailed before this

	verified shaCache // caches the on-disk model file's verified SHA-256

	// Big Brain (ADR-034): rpc is set while the runtime is ggml-rpc-server;
	// preRPC is what ran before it (nil = nothing), restored on leaving.
	rpc    bool
	preRPC *preRPCState
	// prima: the Big Brain runtime is a prima.cpp worker (rpc is set too),
	// with its tunnel to the controller (ADR-037).
	prima  bool
	tunnel *primaTunnel

	desiredCh chan *proto.DesiredRuntime // buffered 1; SetDesired keeps only the latest
}

// preRPCState is the runtime a node served before it joined a Big Brain pool.
type preRPCState struct {
	cfg           RuntimeConfig
	current       string
	residentBytes int64
	lastApplied   *proto.DesiredRuntime
}

// releaseRPC is queued by SetDesired(nil) while the node is in RPC mode: a
// controller that no longer names it as a Big Brain member sends nothing.
var releaseRPC = &proto.DesiredRuntime{Mode: "release"}

func NewManager(cfg ManagerConfig, log *slog.Logger) *Manager {
	return &Manager{
		cfg:       cfg,
		log:       log.With("component", "manager"),
		http:      &http.Client{}, // no fixed timeout: downloads run for as long as ctx allows
		desiredCh: make(chan *proto.DesiredRuntime, 1),
	}
}

// Run starts the static model (if configured) exactly as before, then
// reconciles one desired state at a time until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	if m.cfg.StaticModelPath != "" {
		m.startStatic(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-m.desiredCh:
			if d != nil {
				m.reconcile(ctx, d)
			}
		}
	}
}

func (m *Manager) startStatic(ctx context.Context) {
	rt := NewRuntime(RuntimeConfig{
		ServerBin: m.cfg.ServerBin, ModelPath: m.cfg.StaticModelPath, Port: m.cfg.Port,
		AdvertisePort: m.cfg.AdvertisePort, Threads: m.cfg.Threads, Variant: m.cfg.Variant,
		CtxSize: m.cfg.StaticCtxSize, Slots: 1,
	}, m.log)
	rtCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { rt.Run(rtCtx); close(done) }()
	m.mu.Lock()
	m.runtime, m.cancel, m.done, m.current = rt, cancel, done, ModelName(m.cfg.StaticModelPath)
	m.mu.Unlock()
}

// SetDesired records the controller's latest desired state. nil means "keep
// current runtime" and is a no-op, except in RPC mode, which it leaves (the
// controller names Big Brain members on every heartbeat, ADR-034). Only the
// most recent value is kept; Run's reconcile loop applies one at a time.
func (m *Manager) SetDesired(d *proto.DesiredRuntime) {
	if d == nil {
		m.mu.Lock()
		rpc := m.rpc
		m.mu.Unlock()
		if !rpc {
			return
		}
		d = releaseRPC
	}
	select {
	case <-m.desiredCh:
	default:
	}
	m.desiredCh <- d
}

// Status merges the active Runtime's status (if any) with the Manager's own
// switch-progress fields into one proto.RuntimeStatus for the heartbeat.
func (m *Manager) Status(ctx context.Context) *proto.RuntimeStatus {
	m.mu.Lock()
	rt := m.runtime
	switching, subState, target, progress := m.switching, m.subState, m.target, m.progress
	errMsg, ramBytes, residentBytes, rpc := m.lastErr, m.ramBytes, m.residentBytes, m.rpc
	prima, primaEpoch := m.prima, int64(0)
	if m.lastApplied != nil {
		primaEpoch = m.lastApplied.Prima.Epoch
	}
	m.mu.Unlock()

	var st *proto.RuntimeStatus
	if rt != nil {
		st = rt.Status(ctx)
	} else {
		st = &proto.RuntimeStatus{AdvertisePort: m.cfg.AdvertisePort}
	}
	st.RAMEstimateBytes = ramBytes
	st.ResidentBytes = residentBytes
	st.BudgetBytes = m.budgetBytes()
	st.Error = errMsg
	if switching {
		st.ModelID = target
	}

	if rpc {
		// Never a backend itself: the controller's head serves the model.
		listening := st.Ready
		st.Model, st.ModelID, st.Ready, st.ModelBytes, st.Slots = "", "", false, 0, 0
		switch {
		case errMsg != "":
			st.State = "error"
		case listening && prima && st.Restarts == 0:
			// A worker that restarted on its own is not in the ring anymore:
			// only a new epoch, which restarts every rank, fixes that.
			st.State, st.PrimaEpoch = proto.ModePrima, primaEpoch
		case listening && !prima:
			st.State = proto.ModeRPC
		default:
			st.State = "loading"
		}
		return st
	}
	switch {
	case errMsg != "":
		st.State = "error"
	case switching:
		st.State = subState
		st.Progress = progress
	case rt != nil && st.Ready:
		st.State = "serving"
	case rt != nil:
		st.State = "loading"
	default:
		st.State = "idle"
	}
	return st
}

// reconcile brings the running model in line with d: download (unless
// already cached), size, then load. Skips entirely if d matches the last
// desired state this Manager successfully applied, or if d matches the last
// desired state that failed and its retry backoff has not elapsed (see
// shouldRetryNow).
func (m *Manager) reconcile(ctx context.Context, d *proto.DesiredRuntime) {
	switch {
	case d.Mode == proto.ModeRPC:
		m.enterRPC(ctx, d)
		return
	case d.Mode == proto.ModePrima:
		m.enterPrima(ctx, d)
		return
	case d == releaseRPC:
		m.leaveRPC(ctx)
		return
	}
	m.leaveRPC(ctx) // a model to serve ends RPC mode
	m.mu.Lock()
	if m.lastApplied != nil && *m.lastApplied == *d {
		m.mu.Unlock()
		return
	}
	sameFailure := m.lastFailed != nil && *m.lastFailed == *d
	retryAt, lastFailedBudget := m.retryAt, m.lastFailedBudget
	m.mu.Unlock()
	if sameFailure && !shouldRetryNow(time.Now(), retryAt, lastFailedBudget, m.budgetBytes()) {
		return // still backing off; Status keeps reporting the existing error
	}

	m.mu.Lock()
	m.switching, m.target, m.lastErr = true, d.ModelID, ""
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.switching = false
		m.mu.Unlock()
	}()

	log := m.log.With("model_id", d.ModelID)
	start := time.Now()
	dest, err := m.ensureModel(ctx, d, log)
	if err != nil {
		m.fail(d, err, log)
		return
	}

	fi, err := os.Stat(dest)
	if err != nil {
		m.fail(d, fmt.Errorf("stat %s: %w", dest, err), log)
		return
	}

	budget := m.budgetBytes()
	plan, err := PlanMemory(SizingRequest{
		FileSizeBytes: fi.Size(),
		ResidentBytes: d.ResidentBytes,
		Shape:         ModelShape{Layers: d.Layers, KVHeads: d.KVHeads, HeadDim: d.HeadDim},
		CtxSize:       d.CtxSize,
		Slots:         d.Slots,
		KVType:        d.KVType,
		BudgetBytes:   budget,
	})
	if err != nil {
		if m.serving(d.ModelID) {
			// The model is already running (e.g. started by pcprov -model) and
			// the requested settings do not fit; keep what works instead of
			// reporting an error and retrying on every heartbeat.
			log.Warn("requested runtime settings do not fit; keeping the running instance", "err", err)
			m.mu.Lock()
			m.lastApplied, m.lastErr, m.residentBytes = d, "", d.ResidentBytes
			m.resetBackoffLocked()
			m.mu.Unlock()
			return
		}
		m.fail(d, err, log)
		return
	}
	log.Info("model sized", "ctx_size", plan.CtxSize, "slots", plan.Slots, "kv_type", plan.KVType,
		"ram_estimate_bytes", plan.RAMEstimateBytes, "budget_bytes", budget)
	m.mu.Lock()
	m.ramBytes = plan.RAMEstimateBytes
	m.mu.Unlock()

	m.setSubState("loading")
	if err := m.load(ctx, dest, d, plan); err != nil {
		m.fail(d, err, log)
		return
	}

	m.mu.Lock()
	m.lastApplied, m.lastErr = d, ""
	m.resetBackoffLocked()
	m.mu.Unlock()
	log.Info("model switch complete", "duration_ms", time.Since(start).Milliseconds())
}

// ensureModel downloads d's model unless the verified file is already
// cached, and returns its path.
func (m *Manager) ensureModel(ctx context.Context, d *proto.DesiredRuntime, log *slog.Logger) (string, error) {
	dest := filepath.Join(m.cfg.ModelsDir, d.ModelID+".gguf")
	cached := d.SHA256 != ""
	if cached {
		sha, err := m.verified.verify(dest)
		cached = err == nil && strings.EqualFold(sha, d.SHA256)
	}
	if cached {
		log.Info("model already cached, skipping download")
		return dest, nil
	}
	m.setSubState("downloading")
	if err := m.makeRoom(dest, d.SizeBytes); err != nil {
		return "", err
	}
	dlStart := time.Now()
	url := m.cfg.ControllerURL + d.URL
	if err := downloadModel(ctx, m.http, url, dest, d.SHA256, d.SizeBytes, m.setProgress, log); err != nil {
		return "", err
	}
	log.Info("model download complete", "bytes", d.SizeBytes, "duration_ms", time.Since(dlStart).Milliseconds())
	return dest, nil
}

// resetBackoffLocked clears the retry backoff for a failed desired state
// (the switch it applied to just succeeded, or is no longer wanted). Call
// with mu held.
func (m *Manager) resetBackoffLocked() {
	m.lastFailed, m.backoff, m.retryAt, m.lastFailedBudget = nil, 0, time.Time{}, 0
}

// shouldRetryNow reports whether a switch to the same desired state that
// last failed with lastFailedBudget bytes of budget should be attempted
// again: the backoff window (until retryAt) has elapsed, or budget has grown
// by more than 10% since that failure (e.g. another switch freed RAM), which
// is worth an early retry instead of waiting out a now-stale backoff.
func shouldRetryNow(now, retryAt time.Time, lastFailedBudget, budget int64) bool {
	if !now.Before(retryAt) {
		return true
	}
	return lastFailedBudget > 0 && budget > lastFailedBudget+lastFailedBudget/10
}

// serving reports whether modelID is the model currently running.
func (m *Manager) serving(modelID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtime != nil && m.current == modelID
}

func (m *Manager) setSubState(s string) {
	m.mu.Lock()
	m.subState = s
	m.progress = 0
	m.mu.Unlock()
}

func (m *Manager) setProgress(p float64) {
	m.mu.Lock()
	m.progress = p
	m.mu.Unlock()
}

// fail records d's failure and (re)arms the retry backoff: a repeat of the
// same failed desired state doubles it (capped at maxBackoff), anything else
// (a first failure, or a different desired state) starts it at minBackoff.
func (m *Manager) fail(d *proto.DesiredRuntime, err error, log *slog.Logger) {
	log.Error("model switch failed", "err", err)
	budget := m.budgetBytes()
	m.mu.Lock()
	m.lastErr = fmt.Sprintf("%s: %s", d.ModelID, err)
	if m.lastFailed != nil && *m.lastFailed == *d {
		m.backoff = min(m.backoff*2, maxBackoff)
	} else {
		m.backoff = minBackoff
	}
	m.lastFailed, m.lastFailedBudget = d, budget
	m.retryAt = time.Now().Add(m.backoff)
	m.mu.Unlock()
}

// makeRoom checks free storage for dest and, if short, evicts other cached
// models (LRU, never the one currently served) until it fits.
func (m *Manager) makeRoom(dest string, needed int64) error {
	free, err := freeBytes(m.cfg.ModelsDir)
	if err != nil {
		return fmt.Errorf("statfs %s: %w", m.cfg.ModelsDir, err)
	}
	m.mu.Lock()
	keep := ""
	if m.current != "" {
		keep = filepath.Join(m.cfg.ModelsDir, m.current+".gguf")
	}
	m.mu.Unlock()
	return evictLRU(m.cfg.ModelsDir, keep, dest, free, needed, m.log)
}

// budgetBytes is the memory sizing budget (ADR-012): MemAvailable plus the
// RssAnon of the currently running llama-server (which load will stop),
// minus the configured reserve. RssAnon, not total RSS: see MemoryBudget.
func (m *Manager) budgetBytes() int64 {
	return MemoryBudget(AvailableRAM(), m.currentServerRssAnonBytes(), int64(m.cfg.MemReserveMB)*mib)
}

func (m *Manager) currentServerRssAnonBytes() uint64 {
	m.mu.Lock()
	rt := m.runtime
	m.mu.Unlock()
	if rt == nil {
		return 0
	}
	pid := rt.pid.Load()
	if pid == 0 {
		return 0
	}
	return ParseMeminfo(readFile(fmt.Sprintf("/proc/%d/status", pid)))["RssAnon"]
}

// load stops the current llama-server (if any) and starts the new model,
// waiting for /health. On failure it falls back to the previous model, if
// its file still exists, so the node keeps serving.
func (m *Manager) load(ctx context.Context, dest string, d *proto.DesiredRuntime, plan SizingPlan) error {
	m.mu.Lock()
	prevRuntime, prevCancel, prevDone, prevModelID, prevResidentBytes := m.runtime, m.cancel, m.done, m.current, m.residentBytes
	m.mu.Unlock()

	if prevCancel != nil {
		prevCancel()
		<-prevDone
	}

	rt := NewRuntime(RuntimeConfig{
		ServerBin: m.cfg.ServerBin, ModelPath: dest, Port: m.cfg.Port, AdvertisePort: m.cfg.AdvertisePort,
		Threads: m.cfg.Threads, Variant: m.cfg.Variant, CtxSize: plan.CtxSize, Slots: plan.Slots, KVType: plan.KVType,
	}, m.log)
	rtCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { rt.Run(rtCtx); close(done) }()

	if err := waitHealthy(rtCtx, rt, loadTimeout); err != nil {
		cancel()
		<-done
		return m.fallback(ctx, prevRuntime, prevModelID, prevResidentBytes, d, err)
	}

	now := time.Now()
	_ = os.Chtimes(dest, now, now) // mark as just used, for the LRU eviction order

	m.mu.Lock()
	m.runtime, m.cancel, m.done, m.current, m.residentBytes = rt, cancel, done, d.ModelID, d.ResidentBytes
	m.mu.Unlock()
	return nil
}

// fallback restarts the previous model after a failed switch, if its file is
// still on disk, so the node keeps serving despite the error.
func (m *Manager) fallback(ctx context.Context, prevRuntime *Runtime, prevModelID string, prevResidentBytes int64, d *proto.DesiredRuntime, loadErr error) error {
	if prevRuntime == nil {
		m.clearRuntime()
		return loadErr
	}
	if _, err := os.Stat(prevRuntime.cfg.ModelPath); err != nil {
		m.clearRuntime()
		return loadErr
	}
	fbRuntime := NewRuntime(prevRuntime.cfg, m.log)
	fbCtx, fbCancel := context.WithCancel(ctx)
	fbDone := make(chan struct{})
	go func() { fbRuntime.Run(fbCtx); close(fbDone) }()
	m.mu.Lock()
	m.runtime, m.cancel, m.done, m.current, m.residentBytes = fbRuntime, fbCancel, fbDone, prevModelID, prevResidentBytes
	m.mu.Unlock()
	m.log.Warn("model switch failed, fell back to previous model",
		"model_id", d.ModelID, "fallback_model_id", prevModelID, "err", loadErr)
	return loadErr
}

func (m *Manager) clearRuntime() {
	m.mu.Lock()
	m.runtime, m.cancel, m.done, m.current, m.residentBytes = nil, nil, nil, "", 0
	m.mu.Unlock()
}

// waitHealthy polls rt's /health until it answers 200, ctx ends, or timeout
// elapses.
func waitHealthy(ctx context.Context, rt *Runtime, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		if rt.healthy(ctx) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("llama-server did not become healthy within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// enterRPC replaces the running llama-server with ggml-rpc-server on the
// same port, so the controller's Big Brain head (ADR-034) reaches it over
// the serving port's existing adb forward. The tensor cache holds one model:
// it is wiped when d names another, so phones' flash does not fill up.
func (m *Manager) enterRPC(ctx context.Context, d *proto.DesiredRuntime) {
	m.mu.Lock()
	if m.rpc && m.lastApplied != nil && *m.lastApplied == *d {
		m.mu.Unlock()
		return
	}
	if m.cfg.RPCServerBin == "" {
		if m.lastErr == "" {
			m.log.Error("asked to join a Big Brain pool, but this agent has no ggml-rpc-server")
		}
		m.lastErr = "rpc: no ggml-rpc-server on this node (re-provision with a llama.cpp build that has it)"
		m.mu.Unlock()
		return
	}
	prev, cancel, done, tun := m.runtime, m.cancel, m.done, m.tunnel
	m.tunnel = nil
	if !m.rpc {
		m.preRPC = nil
		if prev != nil {
			m.preRPC = &preRPCState{cfg: prev.cfg, current: m.current, residentBytes: m.residentBytes, lastApplied: m.lastApplied}
		}
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if tun != nil {
		tun.Close()
	}
	cacheModel := filepath.Join(m.cfg.RPCCacheDir, ".model")
	if b, _ := os.ReadFile(cacheModel); strings.TrimSpace(string(b)) != d.ModelID {
		_ = os.RemoveAll(m.cfg.RPCCacheDir)
		if err := os.MkdirAll(m.cfg.RPCCacheDir, 0o755); err == nil {
			_ = os.WriteFile(cacheModel, []byte(d.ModelID+"\n"), 0o644)
		}
	}
	rt := NewRuntime(RuntimeConfig{ServerBin: m.cfg.RPCServerBin, Port: m.cfg.Port, AdvertisePort: m.cfg.AdvertisePort,
		Threads: m.cfg.RPCThreads, Variant: m.cfg.Variant, RPC: true, RPCCacheDir: m.cfg.RPCCacheDir}, m.log)
	rtCtx, cancelRT := context.WithCancel(ctx)
	doneRT := make(chan struct{})
	go func() { rt.Run(rtCtx); close(doneRT) }()
	m.mu.Lock()
	m.runtime, m.cancel, m.done, m.current, m.residentBytes, m.ramBytes = rt, cancelRT, doneRT, "", 0, 0
	m.rpc, m.prima, m.lastApplied, m.lastErr = true, false, d, ""
	m.resetBackoffLocked()
	m.mu.Unlock()
	m.log.Info("joined a Big Brain pool: serving RAM over ggml-rpc-server", "model_id", d.ModelID, "threads", m.cfg.RPCThreads)
}

// leaveRPC stops ggml-rpc-server and restarts what ran before, if anything.
// No-op outside RPC mode.
func (m *Manager) leaveRPC(ctx context.Context) {
	m.mu.Lock()
	if !m.rpc {
		m.mu.Unlock()
		return
	}
	cancel, done, prev, tun := m.cancel, m.done, m.preRPC, m.tunnel
	m.rpc, m.prima, m.tunnel, m.preRPC, m.lastApplied, m.lastErr = false, false, nil, nil, nil, ""
	m.runtime, m.cancel, m.done, m.current, m.residentBytes = nil, nil, nil, "", 0
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if tun != nil {
		tun.Close()
	}
	m.log.Info("left the Big Brain pool")
	if prev == nil {
		return
	}
	rt := NewRuntime(prev.cfg, m.log)
	rtCtx, cancelRT := context.WithCancel(ctx)
	doneRT := make(chan struct{})
	go func() { rt.Run(rtCtx); close(doneRT) }()
	m.mu.Lock()
	m.runtime, m.cancel, m.done, m.current, m.residentBytes, m.lastApplied = rt, cancelRT, doneRT, prev.current, prev.residentBytes, prev.lastApplied
	m.mu.Unlock()
}

// enterPrima makes the node a worker of a prima.cpp ring (ADR-037): it
// downloads the whole model like a normal switch (every rank needs the full
// file, mmap pages in just its layers), then replaces the running runtime by
// the worker and its tunnel to the controller. A new epoch or rank restarts
// the worker: a ring whose member restarted hangs, so all ranks restart
// together.
func (m *Manager) enterPrima(ctx context.Context, d *proto.DesiredRuntime) {
	m.mu.Lock()
	if m.prima && m.lastApplied != nil && *m.lastApplied == *d {
		m.mu.Unlock()
		return
	}
	if m.cfg.PrimaWorkerBin == "" {
		if m.lastErr == "" {
			m.log.Error("asked to join a prima.cpp pool, but this agent has no prima worker")
		}
		m.lastErr = "prima: no prima worker on this node (re-provision with a bin/prima build)"
		m.mu.Unlock()
		return
	}
	sameFailure := m.lastFailed != nil && *m.lastFailed == *d
	retryAt, lastFailedBudget := m.retryAt, m.lastFailedBudget
	m.mu.Unlock()
	if sameFailure && !shouldRetryNow(time.Now(), retryAt, lastFailedBudget, m.budgetBytes()) {
		return
	}
	m.mu.Lock()
	m.switching, m.target, m.lastErr, m.subState, m.progress = true, d.ModelID, "", "loading", 0
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.switching = false
		m.mu.Unlock()
	}()

	log := m.log.With("model_id", d.ModelID)
	dest, err := m.ensureModel(ctx, d, log)
	if err != nil {
		m.fail(d, err, log)
		return
	}
	m.setSubState("loading")

	m.mu.Lock()
	prev, cancel, done, tun := m.runtime, m.cancel, m.done, m.tunnel
	m.tunnel = nil
	if !m.rpc {
		m.preRPC = nil
		if prev != nil {
			m.preRPC = &preRPCState{cfg: prev.cfg, current: m.current, residentBytes: m.residentBytes, lastApplied: m.lastApplied}
		}
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if tun != nil {
		tun.Close()
	}
	dial := func(ctx context.Context, to string) (net.Conn, error) {
		return dialController(ctx, m.cfg.ControllerURL, m.cfg.NodeID, to)
	}
	tunnel, err := startPrimaTunnel(ctx, primaTunnelPorts(m.cfg.Port, d.Prima), dial)
	if err != nil {
		m.mu.Lock()
		m.runtime, m.cancel, m.done, m.current, m.residentBytes = nil, nil, nil, "", 0
		m.rpc, m.prima, m.lastApplied = true, true, nil // preRPC still restores what ran before
		m.mu.Unlock()
		m.fail(d, fmt.Errorf("prima tunnel: %w", err), log)
		return
	}
	rt := NewRuntime(RuntimeConfig{ServerBin: m.cfg.PrimaWorkerBin, ModelPath: dest, Port: m.cfg.Port, AdvertisePort: m.cfg.AdvertisePort,
		Threads: m.cfg.Threads, Variant: m.cfg.Variant, Prima: true, Rank: d.Prima}, m.log)
	rtCtx, cancelRT := context.WithCancel(ctx)
	doneRT := make(chan struct{})
	go func() { rt.Run(rtCtx); close(doneRT) }()
	m.mu.Lock()
	m.runtime, m.cancel, m.done, m.current, m.residentBytes, m.ramBytes = rt, cancelRT, doneRT, "", 0, 0
	m.rpc, m.prima, m.tunnel, m.lastApplied, m.lastErr = true, true, tunnel, d, ""
	m.resetBackoffLocked()
	m.mu.Unlock()
	log.Info("joined a prima.cpp pool: worker started", "rank", d.Prima.Rank, "world", d.Prima.World, "epoch", d.Prima.Epoch)
}
