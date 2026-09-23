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
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

var errUnknownNode = errors.New("controller does not know this node")

type Config struct {
	ControllerURL string
	NodeID        string
	Version       string
	BenchDuration time.Duration
	BenchBufBytes int
}

type Agent struct {
	cfg     Config
	runtime *Runtime // nil when the node serves no model
	http    *http.Client
	log     *slog.Logger
	started time.Time
	bench   *proto.Benchmark // measured once per process, reused on re-registration
}

func New(cfg Config, runtime *Runtime, log *slog.Logger) *Agent {
	return &Agent{cfg: cfg, runtime: runtime, http: &http.Client{Timeout: 10 * time.Second}, log: log, started: time.Now()}
}

// Run registers, benchmarks and heartbeats until ctx is cancelled. Any
// controller loss (unreachable or restarted) leads back to registration with
// capped exponential backoff.
func (a *Agent) Run(ctx context.Context) error {
	props := Props()
	backoff := time.Second
	for ctx.Err() == nil {
		err := a.session(ctx, props)
		if ctx.Err() != nil {
			break
		}
		a.log.Warn("session ended, retrying", "err", err, "backoff", backoff.String())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
		}
		backoff = min(backoff*2, 30*time.Second)
		if errors.Is(err, errUnknownNode) {
			backoff = time.Second
		}
	}
	return ctx.Err()
}

func (a *Agent) session(ctx context.Context, props map[string]string) error {
	inv := Discover(props, a.cfg.Version)
	var resp proto.RegisterResponse
	if err := a.post(ctx, proto.PathRegister, proto.RegisterRequest{NodeID: a.cfg.NodeID, Inventory: inv}, &resp); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	a.log.Info("registered", "node_id", a.cfg.NodeID, "model", inv.Model, "abi", inv.ABI,
		"cores", inv.CPUCores, "ram_bytes", inv.RAMTotalBytes, "heartbeat_sec", resp.HeartbeatIntervalSec)

	// Benchmark before the inference server starts so they don't compete for
	// CPU. Keep the buffer well under available RAM on small phones.
	if a.bench == nil {
		buf := min(a.cfg.BenchBufBytes, int(AvailableRAM()/8))
		b := RunBenchmark(a.cfg.BenchDuration, inv.CPUCores, buf)
		a.bench = &b
		a.log.Info("benchmark done", "kind", b.Kind, "cpu_gflops", b.CPUGFLOPS, "mem_gbps", b.MemBandwidthGBps)
		if a.runtime != nil {
			go a.runtime.Run(ctx)
		}
	}
	if err := a.post(ctx, proto.PathBenchmark, proto.BenchmarkReport{NodeID: a.cfg.NodeID, Benchmark: *a.bench}, nil); err != nil {
		return fmt.Errorf("benchmark report: %w", err)
	}

	interval := time.Duration(max(resp.HeartbeatIntervalSec, 1)) * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()
	failures := 0
	for {
		level, battTemp := Battery()
		hb := proto.Heartbeat{
			NodeID:        a.cfg.NodeID,
			RAMAvailBytes: AvailableRAM(),
			Load1:         Load1(),
			TemperatureC:  MaxTemperature(battTemp),
			BatteryLevel:  level,
			UptimeSec:     int64(time.Since(a.started).Seconds()),
		}
		if a.runtime != nil {
			hb.Runtime = a.runtime.Status(ctx)
		}
		if err := a.post(ctx, proto.PathHeartbeat, hb, nil); err != nil {
			if errors.Is(err, errUnknownNode) {
				return err
			}
			failures++
			a.log.Warn("heartbeat failed", "err", err, "consecutive_failures", failures)
			if failures >= 3 {
				return fmt.Errorf("heartbeat: %w", err)
			}
		} else {
			failures = 0
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (a *Agent) post(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ControllerURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errUnknownNode
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
