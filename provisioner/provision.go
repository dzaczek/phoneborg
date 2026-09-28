package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dzaczek/phoneborg/proto"
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
	// OnStep, if set, is called with the current step name during Provision,
	// in addition to the usual log line; Watch uses it to report progress
	// (docs/DECISIONS.md ADR-019).
	OnStep func(serial, step string)
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
	step := func(name string) {
		log.Info("provision step", "step", name)
		if p.OnStep != nil {
			p.OnStep(serial, name)
		}
	}

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
// that disappears (USB unplugged) is forgotten, so re-plugging re-provisions
// (and resets its backoff, ADR-019). healInterval is how often Watch verifies
// adb links of already-provisioned devices; reportInterval is the same for
// controller reports, when opts.Reporter is set.
const (
	healInterval   = 15 * time.Second
	reportInterval = 15 * time.Second
)

// WatchOptions configures Watch beyond the poll interval.
type WatchOptions struct {
	// Include restricts which serials Watch manages; nil manages every
	// device, emulated ones included.
	Include func(serial string) bool
	// After, if set, runs once right after each successful provision (used
	// to chain slim).
	After func(serial string)
	// Reporter posts the device view to the controller and receives
	// auto_provision and pending operator commands (ADR-019); nil disables
	// reporting, and auto-provisioning behaves as if auto_provision were
	// always true (today's behaviour).
	Reporter *Reporter
}

// Watch is single-threaded by design: one device is provisioned at a time,
// in this loop, so two provisions of the same serial can never race each
// other within one process. Lock (see lock_unix.go) keeps a second pcprov
// process off the same adb server entirely (ADR-019).
func (p *Provisioner) Watch(ctx context.Context, interval time.Duration, opts WatchOptions) error {
	include := opts.Include
	if include == nil {
		include = func(string) bool { return true }
	}
	records := map[string]*deviceRecord{}
	pending := map[string]bool{}  // serial -> operator "provision now"/"retry" command queued
	ackQueue := map[string]bool{} // serials whose pending command was consumed, awaiting a successful report
	lastHeal := map[string]time.Time{}
	autoProvision := true // the controller may turn this off in a report response
	lastReportKey := ""
	var lastReportAt time.Time

	p.OnStep = func(serial, step string) {
		if rec := records[serial]; rec != nil {
			rec.Step = step
		}
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		now := time.Now()
		devs, err := p.ADB.Devices(ctx)
		if err != nil {
			p.Log.Error("adb devices failed", "err", err)
		}
		dup := DuplicateSerials(devs)
		seen := map[string]bool{}
		for _, d := range devs {
			if !include(d.Serial) {
				continue
			}
			seen[d.Serial] = true
			rec := records[d.Serial]
			if rec == nil {
				rec = &deviceRecord{DeviceStatus: proto.DeviceStatus{Serial: d.Serial, FirstSeen: now}}
				records[d.Serial] = rec
			}
			rec.ADBState, rec.Model, rec.LastSeen = d.State, d.Model, now

			if d.State != "device" {
				if rec.Status != proto.DeviceWaitingAuth || rec.Hint != hintForADBState(d.State) {
					p.Log.Warn("device not ready", "serial", d.Serial, "state", d.State)
				}
				rec.Status, rec.Error, rec.Hint = proto.DeviceWaitingAuth, "", hintForADBState(d.State)
				continue
			}

			placeholder := IsPlaceholderSerial(d.Serial)
			plan := planDevice(rec, dup[d.Serial], placeholder, pending[d.Serial], autoProvision, now)
			wasPending := pending[d.Serial]
			rec.Status = plan.Status
			switch {
			case dup[d.Serial]:
				rec.Error, rec.Hint = "another attached device reports this same serial",
					"adb -s "+d.Serial+" is ambiguous; unplug the duplicate or fix its serial, then replug"
			case placeholder:
				rec.Error, rec.Hint = "placeholder serial "+d.Serial,
					"this looks like a generic/junk USB serial, not a real device"
			}
			if !plan.Attempt {
				if wasPending && (dup[d.Serial] || placeholder) {
					delete(pending, d.Serial) // reject the command instead of leaving it queued forever
					ackQueue[d.Serial] = true
				}
				if time.Since(lastHeal[d.Serial]) >= healInterval {
					lastHeal[d.Serial] = time.Now()
					if _, err := p.Heal(ctx, d.Serial); err != nil {
						p.Log.Warn("heal failed", "serial", d.Serial, "err", err)
					}
				}
				continue
			}

			p.Log.Info("provisioning device", "serial", d.Serial, "model", d.Model, "forced", wasPending)
			delete(pending, d.Serial)
			rec.Step, rec.Error, rec.Hint = "", "", ""
			if err := p.Provision(ctx, d.Serial); err != nil {
				p.Log.Error("provision failed", "serial", d.Serial, "err", err, "retry_in", NextBackoff(rec.backoff))
				rec.Status, rec.Error, rec.Hint = proto.DeviceFailed, err.Error(), hintForProvisionError(err)
				rec.backoff = NextBackoff(rec.backoff)
				rec.nextAttempt = now.Add(rec.backoff)
			} else {
				rec.provisioned = true
				rec.Status, rec.backoff, rec.nextAttempt = proto.DeviceProvisioned, 0, time.Time{}
				if opts.After != nil {
					opts.After(d.Serial)
				}
			}
			if wasPending {
				ackQueue[d.Serial] = true
			}
		}
		var gone []string
		for s, rec := range records {
			if !seen[s] && rec.Status != proto.DeviceGone {
				rec.Status, rec.Step, rec.Error, rec.Hint, rec.LastSeen = proto.DeviceGone, "", "", "", now
				p.Log.Info("device gone", "serial", s)
			}
			if !seen[s] {
				gone = append(gone, s)
			}
		}

		if opts.Reporter != nil {
			snapshot := make([]proto.DeviceStatus, 0, len(records))
			for _, rec := range records {
				snapshot = append(snapshot, rec.DeviceStatus)
			}
			sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].Serial < snapshot[j].Serial })
			key := reportKey(snapshot)
			if key != lastReportKey || now.Sub(lastReportAt) >= reportInterval {
				acked := make([]string, 0, len(ackQueue))
				for s := range ackQueue {
					acked = append(acked, s)
				}
				resp, err := opts.Reporter.Report(ctx, snapshot, acked)
				lastReportAt = time.Now()
				if err != nil {
					p.Log.Warn("device report failed", "err", err)
				} else {
					autoProvision = resp.AutoProvision
					for _, cmd := range resp.Commands {
						pending[cmd.Serial] = true
					}
					for _, s := range acked {
						delete(ackQueue, s)
					}
					lastReportKey = key
					for _, s := range gone {
						delete(records, s) // reported as "gone" above; drop it now
					}
				}
			}
		} else {
			for _, s := range gone {
				delete(records, s)
			}
		}
		for _, s := range gone {
			delete(lastHeal, s)
		}

		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// reportKey summarises a (serial-sorted) device snapshot for change
// detection: it ignores timestamps, so a report is not forced on every tick
// just because LastSeen advanced, only on a real state change (or the
// reportInterval floor).
func reportKey(snapshot []proto.DeviceStatus) string {
	var b strings.Builder
	for _, d := range snapshot {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%s\n", d.Serial, d.ADBState, d.Status, d.Step, d.Error, d.Hint)
	}
	return b.String()
}
