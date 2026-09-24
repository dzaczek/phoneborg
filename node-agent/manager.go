// Model switching (ADR-012): the Manager reconciles the model the controller
// wants (proto.DesiredRuntime, from a heartbeat response) with the
// llama-server the Runtime supervises, downloading, sizing and loading a new
// model one switch at a time.
package nodeagent

import (
	"context"
	"fmt"
	"log/slog"
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

	desiredCh chan *proto.DesiredRuntime // buffered 1; SetDesired keeps only the latest
}

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
// current runtime" and is a no-op. Only the most recent value is kept; Run's
// reconcile loop applies one at a time.
func (m *Manager) SetDesired(d *proto.DesiredRuntime) {
	if d == nil {
		return
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
	errMsg, ramBytes, residentBytes := m.lastErr, m.ramBytes, m.residentBytes
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
	dest := filepath.Join(m.cfg.ModelsDir, d.ModelID+".gguf")

	cached := d.SHA256 != ""
	if cached {
		sha, err := m.verified.verify(dest)
		cached = err == nil && strings.EqualFold(sha, d.SHA256)
	}
	if cached {
		log.Info("model already cached, skipping download")
	} else {
		m.setSubState("downloading")
		if err := m.makeRoom(dest, d.SizeBytes); err != nil {
			m.fail(d, err, log)
			return
		}
		dlStart := time.Now()
		url := m.cfg.ControllerURL + d.URL
		if err := downloadModel(ctx, m.http, url, dest, d.SHA256, d.SizeBytes, m.setProgress, log); err != nil {
			m.fail(d, err, log)
			return
		}
		log.Info("model download complete", "bytes", d.SizeBytes, "duration_ms", time.Since(dlStart).Milliseconds())
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
