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
	llamaServer := flag.String("llama-server", "", "llama-server binary; enables serving (requires -model)")
	model := flag.String("model", "", "GGUF model to serve")
	servePort := flag.Int("serve-port", 18090, "on-device port for llama-server (127.0.0.1)")
	advertisePort := flag.Int("advertise-port", 0, "host-side port that reaches -serve-port (set by pcprov via adb forward)")
	ctxSize := flag.Int("ctx-size", 2048, "llama-server context size")
	runtimeVariant := flag.String("runtime-variant", "", "llama.cpp build variant (set by pcprov); reported as the engine name")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	props := nodeagent.Props()
	nodeID := *id
	if nodeID == "" {
		nodeID = nodeagent.NodeID(props)
	}
	var runtime *nodeagent.Runtime
	if *llamaServer != "" {
		if *model == "" || *advertisePort == 0 {
			log.Error("-llama-server requires -model and -advertise-port")
			os.Exit(2)
		}
		runtime = nodeagent.NewRuntime(nodeagent.RuntimeConfig{
			ServerBin: *llamaServer, ModelPath: *model, Port: *servePort, AdvertisePort: *advertisePort,
			Threads: nodeagent.Discover(props, version).CPUCores, CtxSize: *ctxSize, Variant: *runtimeVariant,
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
	}, runtime, log)
	_ = a.Run(ctx)
	log.Info("node-agent stopped")
}
