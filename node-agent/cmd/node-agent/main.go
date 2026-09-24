// Command node-agent runs on the phone (as the adb shell user) and joins the
// controller. It is pushed and started by pcprov.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	nodeagent "github.com/dzaczek/phoneborg/node-agent"
)

var version = "dev"

func main() {
	ctrl := flag.String("controller", "http://127.0.0.1:18080", "controller URL (127.0.0.1 works over USB via adb reverse)")
	id := flag.String("node-id", "", "node ID override (default: serial / android_id)")
	benchDur := flag.Duration("bench-duration", 3*time.Second, "duration of each benchmark phase")
	benchBuf := flag.Int("bench-buffer-mb", 64, "max memory for bandwidth benchmark (MiB)")
	llamaServer := flag.String("llama-server", "", "llama-server binary; enables serving (a static -model, a controller-desired model, or both)")
	model := flag.String("model", "", "GGUF model to serve until/unless the controller sends a desired model (ADR-012)")
	servePort := flag.Int("serve-port", 18090, "on-device port for llama-server (127.0.0.1)")
	advertisePort := flag.Int("advertise-port", 0, "host-side port that reaches -serve-port (set by pcprov via adb forward)")
	ctxSize := flag.Int("ctx-size", 2048, "llama-server context size (for the static -model only; a controller-desired model is sized automatically, see ADR-012)")
	threadsOverride := flag.Int("threads", 0, "llama-server thread count override (0 = choose automatically from CPU topology, see -threads-policy)")
	threadsPolicy := flag.String("threads-policy", "all", `automatic thread selection when -threads=0: "all" (every allowed CPU, today's behavior) or "big" (only the highest-frequency CPU cluster)`)
	runtimeVariant := flag.String("runtime-variant", "", "llama.cpp build variant (set by pcprov); reported as the engine name")
	memReserveMB := flag.Int("mem-reserve-mb", 600, "RAM reserved for Android and the agent when sizing a controller-desired model (ADR-012)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	props := nodeagent.Props()
	nodeID := *id
	if nodeID == "" {
		nodeID = nodeagent.NodeID(props)
	}
	var manager *nodeagent.Manager
	if *llamaServer != "" {
		if *advertisePort == 0 {
			log.Error("-llama-server requires -advertise-port")
			os.Exit(2)
		}
		if err := os.MkdirAll(modelsDir, 0o755); err != nil {
			log.Error("create models dir", "dir", modelsDir, "err", err)
			os.Exit(2)
		}
		cpuCores := nodeagent.Discover(props, version).CPUCores
		threads, allowedCPUs, bigCores := nodeagent.DiscoverThreads(*threadsPolicy)
		if threads <= 0 {
			threads = cpuCores
		}
		if *threadsOverride > 0 {
			threads = *threadsOverride
		}
		if threads > cpuCores {
			threads = cpuCores // respect the cgroup CPU cap
		}
		log.Info("runtime threads selected", "threads", threads, "policy", *threadsPolicy,
			"allowed_cpus", allowedCPUs, "big_cores", bigCores)
		manager = nodeagent.NewManager(nodeagent.ManagerConfig{
			ServerBin: *llamaServer, Port: *servePort, AdvertisePort: *advertisePort,
			Threads: threads, Variant: *runtimeVariant, ModelsDir: modelsDir, ControllerURL: *ctrl,
			MemReserveMB: *memReserveMB, StaticModelPath: *model, StaticCtxSize: *ctxSize,
		}, log)
	}
	log.Info("node-agent starting", "version", version, "node_id", nodeID, "controller", *ctrl, "pid", os.Getpid())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a := nodeagent.New(nodeagent.Config{
		ControllerURL: *ctrl,
		NodeID:        nodeID,
		Version:       version,
		BenchDuration: *benchDur,
		BenchBufBytes: *benchBuf << 20,
	}, manager, log)
	_ = a.Run(ctx)
	log.Info("node-agent stopped")
}

// modelsDir is where the agent caches GGUF files, under its working dir
// (/data/local/tmp/phoneborg/models on phones, see ADR-012).
const modelsDir = "models"
