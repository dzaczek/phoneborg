package nodeagent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

// RuntimeConfig describes the local inference server the agent supervises.
type RuntimeConfig struct {
	ServerBin     string // llama-server binary
	ModelPath     string // .gguf file
	Port          int    // on-device listen port (127.0.0.1 only)
	AdvertisePort int    // host-side port that reaches Port (adb forward)
	Threads       int
	CtxSize       int
}

// ModelName is the model identifier clients use: the file name without .gguf.
func ModelName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".gguf")
}

// Runtime keeps llama-server running and reports its readiness.
type Runtime struct {
	cfg      RuntimeConfig
	log      *slog.Logger
	restarts atomic.Int64
	http     *http.Client
}

func NewRuntime(cfg RuntimeConfig, log *slog.Logger) *Runtime {
	return &Runtime{cfg: cfg, log: log.With("component", "runtime"), http: &http.Client{Timeout: 2 * time.Second}}
}

const runtimePidFile = "runtime.pid"

// Run starts llama-server and restarts it with backoff until ctx ends.
func (r *Runtime) Run(ctx context.Context) {
	killStale(r.log)
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := r.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		r.restarts.Add(1)
		r.log.Error("llama-server exited, restarting", "err", err, "backoff", backoff.String(), "restarts", r.restarts.Load())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (r *Runtime) runOnce(ctx context.Context) error {
	args := []string{
		"-m", r.cfg.ModelPath, "-a", ModelName(r.cfg.ModelPath),
		"-t", strconv.Itoa(r.cfg.Threads), "-c", strconv.Itoa(r.cfg.CtxSize), "-np", "1",
		"--host", "127.0.0.1", "--port", strconv.Itoa(r.cfg.Port),
	}
	logf, err := os.OpenFile("runtime.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.CommandContext(ctx, r.cfg.ServerBin, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", r.cfg.ServerBin, err)
	}
	_ = os.WriteFile(runtimePidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	defer os.Remove(runtimePidFile)
	r.log.Info("llama-server started", "pid", cmd.Process.Pid, "model", ModelName(r.cfg.ModelPath), "port", r.cfg.Port)
	return cmd.Wait()
}

// killStale stops a llama-server left behind by a SIGKILLed agent, which
// would otherwise hold the port. The cmdline check guards against PID reuse.
func killStale(log *slog.Logger) {
	b, err := os.ReadFile(runtimePidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return
	}
	if cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); strings.Contains(string(cmdline), "llama-server") {
		log.Warn("killing stale llama-server", "pid", pid)
		_ = syscall.Kill(pid, syscall.SIGKILL)
		time.Sleep(500 * time.Millisecond)
	}
	os.Remove(runtimePidFile)
}

// Status probes llama-server's /health (200 only once the model is loaded).
func (r *Runtime) Status(ctx context.Context) *proto.RuntimeStatus {
	st := &proto.RuntimeStatus{
		Engine:        "llama.cpp",
		Model:         ModelName(r.cfg.ModelPath),
		AdvertisePort: r.cfg.AdvertisePort,
		Restarts:      r.restarts.Load(),
		Threads:       r.cfg.Threads,
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", r.cfg.Port), nil)
	if resp, err := r.http.Do(req); err == nil {
		resp.Body.Close()
		st.Ready = resp.StatusCode == http.StatusOK
	}
	return st
}
