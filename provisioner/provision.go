package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const RemoteDir = "/data/local/tmp/phoneborg"

type Options struct {
	AgentBinary    string // local path to the android/arm64 node-agent
	ControllerPort int    // host port exposed to the phone via adb reverse
	AgentArgs      string // extra flags passed to node-agent
	LlamaServer    string // explicit llama-server binary override; skips auto-selection when set
	LlamaDir       string // directory of ARM_ARCH variant subdirs (see ADR-007), used when LlamaServer is empty
	Model          string // local .gguf to serve; empty = inventory/heartbeat only
	ServePort      int    // on-device llama-server port
}

type Provisioner struct {
	ADB  ADB
	Opts Options
	Log  *slog.Logger
}

// SupportedABI reports whether the device can run the arm64 agent.
func SupportedABI(abiList string) bool {
	for _, abi := range strings.Split(abiList, ",") {
		if strings.TrimSpace(abi) == "arm64-v8a" {
			return true
		}
	}
	return false
}

// Provision is idempotent: it replaces any running agent with the pushed one.
func (p *Provisioner) Provision(ctx context.Context, serial string) error {
	log := p.Log.With("serial", serial)
	step := func(name string) { log.Info("provision step", "step", name) }

	step("check-abi")
	abis, err := p.ADB.Shell(ctx, serial, "getprop ro.product.cpu.abilist")
	if err != nil {
		return err
	}
	if !SupportedABI(abis) {
		return fmt.Errorf("device %s: unsupported ABI list %q (need arm64-v8a)", serial, strings.TrimSpace(abis))
	}

	step("adb-reverse")
	if err := p.ADB.Reverse(ctx, serial, p.Opts.ControllerPort); err != nil {
		return err
	}

	step("push")
	if _, err := p.ADB.Shell(ctx, serial, "mkdir -p "+RemoteDir); err != nil {
		return err
	}
	if err := p.ADB.Push(ctx, serial, p.Opts.AgentBinary, RemoteDir+"/node-agent.new"); err != nil {
		return err
	}

	args := p.Opts.AgentArgs
	if p.Opts.Model != "" {
		step("push-runtime")
		serveArgs, err := p.pushRuntime(ctx, serial)
		if err != nil {
			return err
		}
		args += serveArgs
	}

	step("restart-agent")
	// Swap binary atomically, stop the old agent, start detached so it
	// survives the adb shell session ending. A pidfile is used instead of
	// `pkill -f`, which would match (and kill) this very shell command.
	start := fmt.Sprintf(
		"cd %[1]s && chmod 755 node-agent.new && mv -f node-agent.new node-agent && %[2]s; "+
			"setsid nohup ./node-agent --controller http://127.0.0.1:%[3]d %[4]s </dev/null >agent.log 2>&1 & "+
			"echo $! > agent.pid; sleep 1; kill -0 $(cat agent.pid) && cat agent.pid",
		RemoteDir, killAgent, p.Opts.ControllerPort, args)
	out, err := p.ADB.Shell(ctx, serial, start)
	pid := strings.TrimSpace(out)
	if err != nil || pid == "" {
		logTail, _ := p.ADB.Shell(ctx, serial, "tail -n 20 "+RemoteDir+"/agent.log")
		return fmt.Errorf("device %s: agent did not start (err=%v): %s", serial, err, logTail)
	}
	log.Info("agent running", "pid", pid)
	return nil
}

// pushRuntime installs llama-server and the model (skipped when a file of the
// same size is already there), forwards the serving port and returns the
// node-agent flags that enable serving.
func (p *Provisioner) pushRuntime(ctx context.Context, serial string) (string, error) {
	llamaServer, variant, err := p.selectLlamaServer(ctx, serial)
	if err != nil {
		return "", err
	}
	if _, err := p.ADB.Shell(ctx, serial, "mkdir -p "+RemoteDir+"/bin "+RemoteDir+"/models"); err != nil {
		return "", err
	}
	if err := p.ADB.Push(ctx, serial, llamaServer, RemoteDir+"/bin/llama-server"); err != nil {
		return "", err
	}
	remoteModel := RemoteDir + "/models/" + filepath.Base(p.Opts.Model)
	st, err := os.Stat(p.Opts.Model)
	if err != nil {
		return "", err
	}
	size, _ := p.ADB.Shell(ctx, serial, "stat -c%s "+remoteModel+" 2>/dev/null; true")
	if strings.TrimSpace(size) != fmt.Sprint(st.Size()) {
		p.Log.Info("pushing model", "serial", serial, "model", filepath.Base(p.Opts.Model), "bytes", st.Size())
		// Longer timeout: a GB-sized model over USB 2.0 takes a while.
		slow := p.ADB
		slow.Timeout = 30 * time.Minute
		if err := slow.Push(ctx, serial, p.Opts.Model, remoteModel); err != nil {
			return "", err
		}
	}
	if _, err := p.ADB.Shell(ctx, serial, "chmod 755 "+RemoteDir+"/bin/llama-server"); err != nil {
		return "", err
	}
	hostPort, err := p.ADB.Forward(ctx, serial, p.Opts.ServePort)
	if err != nil {
		return "", err
	}
	p.Log.Info("serving port forwarded", "serial", serial, "host_port", hostPort, "device_port", p.Opts.ServePort)
	variantArg := ""
	if variant != "" {
		variantArg = " --runtime-variant " + variant
	}
	return fmt.Sprintf(" --llama-server bin/llama-server --model models/%s --serve-port %d --advertise-port %d%s",
		filepath.Base(p.Opts.Model), p.Opts.ServePort, hostPort, variantArg), nil
}

// selectLlamaServer returns the local llama-server binary to push to serial
// and the variant name to report to the controller. When Opts.LlamaServer is
// set explicitly, it is used as-is and auto-selection is skipped (variant is
// then reported as empty, i.e. the plain "llama.cpp" engine name). Otherwise
// the phone's CPU features are read over adb and matched against the builds
// available under Opts.LlamaDir (see ADR-007).
func (p *Provisioner) selectLlamaServer(ctx context.Context, serial string) (path, variant string, err error) {
	if p.Opts.LlamaServer != "" {
		return p.Opts.LlamaServer, "", nil
	}
	out, err := p.ADB.Shell(ctx, serial, "grep -m1 -i '^features' /proc/cpuinfo")
	if err != nil {
		return "", "", fmt.Errorf("device %s: read CPU features: %w", serial, err)
	}
	available := AvailableLlamaVariants(p.Opts.LlamaDir)
	variant, err = SelectLlamaVariant(ParseCPUFeatures(out), available)
	if err != nil {
		return "", "", fmt.Errorf("device %s: %w", serial, err)
	}
	p.Log.Info("llama variant selected", "serial", serial, "variant", variant, "features", strings.TrimSpace(out))
	return filepath.Join(p.Opts.LlamaDir, variant, "llama-server"), variant, nil
}

// killAgent stops the agent recorded in the pidfile (run from RemoteDir).
const killAgent = "if [ -f agent.pid ]; then kill $(cat agent.pid) 2>/dev/null; rm -f agent.pid; sleep 0.3; fi"

func (p *Provisioner) Stop(ctx context.Context, serial string) error {
	_, err := p.ADB.Shell(ctx, serial, "cd "+RemoteDir+" 2>/dev/null && "+killAgent+"; true")
	return err
}

func (p *Provisioner) Status(ctx context.Context, serial string) (string, error) {
	return p.ADB.Shell(ctx, serial, "cd "+RemoteDir+" 2>/dev/null; "+
		"if [ -f agent.pid ] && kill -0 $(cat agent.pid) 2>/dev/null; then echo running pid=$(cat agent.pid); else echo stopped; fi; "+
		"tail -n 5 agent.log 2>/dev/null")
}

// Watch provisions every device that reaches the "device" state. A device
// that disappears (USB unplugged) is forgotten, so re-plugging re-provisions.
func (p *Provisioner) Watch(ctx context.Context, interval time.Duration, include func(serial string) bool) error {
	done := map[string]bool{}
	warned := map[string]string{}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		devs, err := p.ADB.Devices(ctx)
		if err != nil {
			p.Log.Error("adb devices failed", "err", err)
		}
		seen := map[string]bool{}
		for _, d := range devs {
			if !include(d.Serial) {
				continue
			}
			seen[d.Serial] = true
			if d.State != "device" {
				if warned[d.Serial] != d.State {
					p.Log.Warn("device not ready", "serial", d.Serial, "state", d.State,
						"hint", "unauthorized: accept the USB debugging prompt on the phone")
					warned[d.Serial] = d.State
				}
				continue
			}
			if done[d.Serial] {
				continue
			}
			p.Log.Info("new device", "serial", d.Serial, "model", d.Model)
			if err := p.Provision(ctx, d.Serial); err != nil {
				p.Log.Error("provision failed", "serial", d.Serial, "err", err)
				continue // retried next tick
			}
			done[d.Serial] = true
		}
		for s := range done {
			if !seen[s] {
				p.Log.Info("device gone", "serial", s)
				delete(done, s)
				delete(warned, s)
			}
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
