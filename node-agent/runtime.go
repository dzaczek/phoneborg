package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	CtxSize       int    // context per slot; llama-server's -c is CtxSize*Slots (see runOnce)
	Slots         int    // parallel slots (-np); <=0 is treated as 1
	KVType        string // "" or "f16" (default), or "q8_0" (adds -ctk/-ctv/-fa, see ADR-012)
	Variant       string // llama.cpp build variant selected by pcprov (ADR-007); empty if unknown
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
	pid      atomic.Int32 // 0 when no llama-server process is running

	mu         sync.Mutex // guards the self-test result below
	genTPS     float64
	promptTPS  float64
	selfTestAt time.Time
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
	slots := max(r.cfg.Slots, 1)
	// llama-server's -c/--ctx-size is the TOTAL context across all -np
	// slots (each slot gets ctx-size/n_parallel), not the per-slot size, so
	// the agent multiplies CtxSize (per slot) by the slot count here.
	args := []string{
		"-m", r.cfg.ModelPath, "-a", ModelName(r.cfg.ModelPath),
		"-t", strconv.Itoa(r.cfg.Threads), "-c", strconv.Itoa(r.cfg.CtxSize * slots), "-np", strconv.Itoa(slots),
		"--host", "127.0.0.1", "--port", strconv.Itoa(r.cfg.Port),
	}
	if r.cfg.KVType == "q8_0" {
		// q8_0 V-cache quantization requires flash attention.
		args = append(args, "-ctk", "q8_0", "-ctv", "q8_0", "-fa", "on")
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
	r.pid.Store(int32(cmd.Process.Pid))
	defer r.pid.Store(0)
	_ = os.WriteFile(runtimePidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	defer os.Remove(runtimePidFile)
	r.log.Info("llama-server started", "pid", cmd.Process.Pid, "model", ModelName(r.cfg.ModelPath),
		"port", r.cfg.Port, "ctx_size", r.cfg.CtxSize, "slots", slots, "kv_type", r.cfg.KVType)
	selfTestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go r.waitReadyThenSelfTest(selfTestCtx)
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
	engine := "llama.cpp"
	if r.cfg.Variant != "" {
		engine = "llama.cpp/" + r.cfg.Variant
	}
	st := &proto.RuntimeStatus{
		Engine:        engine,
		Model:         ModelName(r.cfg.ModelPath),
		ModelID:       ModelName(r.cfg.ModelPath),
		Slots:         max(r.cfg.Slots, 1),
		KVType:        r.cfg.KVType,
		AdvertisePort: r.cfg.AdvertisePort,
		Restarts:      r.restarts.Load(),
		Threads:       r.cfg.Threads,
		CtxSize:       r.cfg.CtxSize,
		Ready:         r.healthy(ctx),
	}
	if fi, err := os.Stat(r.cfg.ModelPath); err == nil {
		st.ModelBytes = fi.Size()
	}
	r.mu.Lock()
	st.GenTPS, st.PromptTPS, st.SelfTestAt = r.genTPS, r.promptTPS, r.selfTestAt
	r.mu.Unlock()
	return st
}

// healthy probes llama-server's /health (200 only once the model is loaded).
func (r *Runtime) healthy(ctx context.Context) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", r.cfg.Port), nil)
	resp, err := r.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// waitReadyThenSelfTest polls llama-server until it is ready (or ctx ends,
// e.g. the process restarted) and then runs one self-test.
func (r *Runtime) waitReadyThenSelfTest(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if r.healthy(ctx) {
				r.runSelfTest(ctx)
				return
			}
		}
	}
}

// selfTestPrompt is a fixed ~100-token prompt used to measure real generation
// speed against the node's own llama-server (see ADR-010).
const selfTestPrompt = `Describe, in a few plain sentences, how a small cluster of old Android ` +
	`phones could be used to serve a large language model to multiple users at once. Mention how ` +
	`requests might be distributed across phones, what happens if one phone becomes slow or ` +
	`unavailable, and why keeping related requests on the same phone could help performance. Keep ` +
	`the explanation concise and easy to follow for someone new to the idea of distributed inference ` +
	`on constrained hardware.`

type selfTestResult struct {
	PromptTPS float64
	GenTPS    float64
}

// runSelfTest posts a fixed prompt to llama-server's own OpenAI-compatible
// endpoint and reads the measured speed from its `timings`. cache_prompt is
// disabled and the prompt is fixed, so repeated self-tests measure cold
// performance every time, not a warm cache.
func selfTestRequest(ctx context.Context, client *http.Client, baseURL, model string) (selfTestResult, error) {
	body, err := json.Marshal(struct {
		Model       string              `json:"model"`
		Messages    []map[string]string `json:"messages"`
		MaxTokens   int                 `json:"max_tokens"`
		IgnoreEOS   bool                `json:"ignore_eos"`
		CachePrompt bool                `json:"cache_prompt"`
		Temperature float64             `json:"temperature"`
	}{
		Model:       model,
		Messages:    []map[string]string{{"role": "user", "content": selfTestPrompt}},
		MaxTokens:   32,
		IgnoreEOS:   true,
		CachePrompt: false,
		Temperature: 0,
	})
	if err != nil {
		return selfTestResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return selfTestResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return selfTestResult{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return selfTestResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return selfTestResult{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}
	return parseSelfTest(respBody)
}

// parseSelfTest extracts prompt/generation speed from a llama-server
// /v1/chat/completions response body. Pure function, no I/O.
func parseSelfTest(body []byte) (selfTestResult, error) {
	var v struct {
		Timings *struct {
			PromptPerSecond    float64 `json:"prompt_per_second"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return selfTestResult{}, err
	}
	if v.Timings == nil {
		return selfTestResult{}, errors.New("response has no timings")
	}
	return selfTestResult{PromptTPS: v.Timings.PromptPerSecond, GenTPS: v.Timings.PredictedPerSecond}, nil
}

// runSelfTestClient bounds one self-test call: prompt processing plus 32
// generated tokens should finish well within this even on a slow phone.
const runSelfTestTimeout = 60 * time.Second

// runSelfTest runs the self-test against this runtime's own llama-server and
// stores the result. Logs the outcome either way; a failure leaves the fields
// at their previous value (zero if none has succeeded yet) and does not
// affect serving.
func (r *Runtime) runSelfTest(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, runSelfTestTimeout)
	defer cancel()
	client := &http.Client{Timeout: runSelfTestTimeout}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", r.cfg.Port)
	res, err := selfTestRequest(ctx, client, baseURL, ModelName(r.cfg.ModelPath))
	if err != nil {
		r.log.Warn("runtime self-test failed", "err", err)
		return
	}
	r.mu.Lock()
	r.genTPS, r.promptTPS, r.selfTestAt = res.GenTPS, res.PromptTPS, time.Now()
	r.mu.Unlock()
	r.log.Info("runtime self-test", "gen_tps", res.GenTPS, "prompt_tps", res.PromptTPS)
}
