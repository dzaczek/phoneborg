// Command pcprov provisions Android phones into a PhoneBorg over ADB.
//
//	pcprov devices
//	pcprov provision -serial <serial> | -all
//	pcprov watch [-scan <ip|cidr|a-b>]...   # auto-provision phones as they are plugged in; -scan also finds network-adb boxes
//	pcprov status -serial <serial>
//	pcprov stop   -serial <serial>
//	pcprov slim   -serial <serial> | -all    # disable non-essential apps to free RAM
//	pcprov unslim -serial <serial> | -all    # restore what slim disabled
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/dzaczek/phoneborg/provisioner"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func usage() {
	fmt.Fprintln(os.Stderr, "usage: pcprov <devices|provision|watch|status|stop|heal|slim|unslim> [flags]\n  run 'pcprov <cmd> -h' for flags\n  docs and source: https://github.com/dzaczek/phoneborg")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	adbBin := fs.String("adb", "adb", "adb binary")
	agent := fs.String("agent", "bin/node-agent-android-arm64", "node-agent binary to push")
	port := fs.Int("controller-port", 18080, "controller port on this host (exposed to phones via adb reverse)")
	agentArgs := fs.String("agent-args", "", "extra node-agent flags")
	model := fs.String("model", "", "GGUF model to serve on each phone (empty: no serving)")
	llamaServer := fs.String("llama-server", "", "explicit arm64 llama-server binary; overrides auto-selection from -llama-dir when set")
	llamaDir := fs.String("llama-dir", "bin/llama", "directory of llama.cpp ARM_ARCH variant builds (make llama-all); the best one for each phone's CPU is picked automatically")
	primaDir := fs.String("prima-dir", "bin/prima", "directory of prima.cpp variant builds named like -llama-dir's (make prima-all); a phone gets the worker of its llama variant, and can then join prima.cpp Big Brain pools (ADR-037)")
	servePort := fs.Int("serve-port", 18090, "on-device llama-server port")
	all := fs.Bool("all", false, "provision all ready devices")
	slimFlag := fs.Bool("slim", false, "run slim after a successful provision (provision, watch)")
	slimTelephony := fs.Bool("slim-telephony", false, "slim/unslim: also disable dialer/contacts (off by default)")
	controllerURL := fs.String("controller-url", "http://127.0.0.1:18080", "controller URL for device reporting (watch only, ADR-019)")
	adminTokenFile := fs.String("admin-token-file", "", "admin token file for device reporting (watch only); empty = reporting disabled")
	scanPort := fs.Int("scan-port", 5555, "adb port probed on -scan targets (watch only)")
	scanInterval := fs.Duration("scan-interval", 2*time.Minute, "how often -scan targets are probed (watch only)")
	var serials, connects, scans multi
	fs.Var(&serials, "serial", "device serial (repeatable); for watch, restricts to these serials")
	fs.Var(&connects, "connect", "adb connect host:port first (repeatable), e.g. emulated phones")
	fs.Var(&scans, "scan", "watch only: IP, IP:port, CIDR or range (a.b.c.d-e or a.b.c.d-a.b.c.e) probed for network adb, e.g. TV boxes (repeatable)")
	_ = fs.Parse(os.Args[2:])

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	adb := provisioner.ADB{Bin: *adbBin, Timeout: 60 * time.Second}
	p := &provisioner.Provisioner{ADB: adb, Log: log, Opts: provisioner.Options{
		AgentBinary: *agent, ControllerPort: *port, AgentArgs: *agentArgs,
		Model: *model, LlamaServer: *llamaServer, LlamaDir: *llamaDir, PrimaDir: *primaDir, ServePort: *servePort}}

	for _, c := range connects {
		if err := adb.Connect(ctx, c); err != nil {
			log.Warn("adb connect failed", "target", c, "err", err)
		}
		serials = append(serials, c)
	}

	if cmd == "provision" || cmd == "watch" {
		if _, err := os.Stat(*agent); err != nil {
			log.Error("agent binary missing, run 'make agent'", "path", *agent)
			os.Exit(1)
		}
		if *model != "" {
			if _, err := os.Stat(*model); err != nil {
				log.Error("serving file missing (download model)", "path", *model)
				os.Exit(1)
			}
			if *llamaServer != "" {
				if _, err := os.Stat(*llamaServer); err != nil {
					log.Error("serving file missing (make llama)", "path", *llamaServer)
					os.Exit(1)
				}
			} else if len(provisioner.AvailableLlamaVariants(*llamaDir)) == 0 {
				log.Error("no llama.cpp build found, run 'make llama-all'", "path", *llamaDir)
				os.Exit(1)
			}
		}
		// Only watch and provision drive devices; read-only commands (devices,
		// status, ...) do not need exclusivity (ADR-019).
		lockPath := provisioner.LockPath(provisioner.ADBServerPort())
		release, err := provisioner.Lock(lockPath)
		if err != nil {
			log.Error(err.Error())
			os.Exit(1)
		}
		defer release()
	}

	var scanTargets []string
	if cmd == "watch" && len(scans) > 0 {
		var err error
		scanTargets, err = provisioner.ParseScanTargets(scans, *scanPort)
		if err != nil {
			log.Error(err.Error())
			os.Exit(1)
		}
		if *scanInterval <= 0 {
			log.Error("-scan-interval must be positive", "got", *scanInterval)
			os.Exit(1)
		}
	}

	switch cmd {
	case "devices":
		devs, err := adb.Devices(ctx)
		fail(log, err)
		for _, d := range devs {
			fmt.Printf("%-24s %-14s %s\n", d.Serial, d.State, d.Model)
		}
	case "provision":
		targets := resolveTargets(ctx, log, adb, *all, serials)
		failed := 0
		for _, s := range targets {
			if err := p.Provision(ctx, s); err != nil {
				log.Error("provision failed", "serial", s, "err", err)
				failed++
				continue
			}
			if *slimFlag {
				if err := p.Slim(ctx, s, *slimTelephony); err != nil {
					log.Error("slim failed", "serial", s, "err", err)
					failed++
				}
			}
		}
		if failed > 0 {
			os.Exit(1)
		}
	case "watch":
		allowed := map[string]bool{}
		for _, s := range serials {
			allowed[s] = true
		}
		var afterProvision func(serial string)
		if *slimFlag {
			afterProvision = func(serial string) {
				if err := p.Slim(ctx, serial, *slimTelephony); err != nil {
					log.Error("slim failed", "serial", serial, "err", err)
				}
			}
		}
		var reporter *provisioner.Reporter
		if *adminTokenFile != "" {
			tok, err := controllerToken(*adminTokenFile)
			if err != nil {
				log.Error("loading admin token", "err", err)
				os.Exit(1)
			}
			reporter = provisioner.NewReporter(*controllerURL, tok)
			log.Info("reporting devices to controller", "url", *controllerURL, "host", reporter.Host)
		} else {
			log.Info("device reporting disabled; pass -admin-token-file to show devices in the web panel (ADR-019)")
		}
		if len(scanTargets) > 0 {
			// Start the adb server here, once: otherwise the scanner and the
			// first Watch tick both spawn it and one fails with "Address
			// already in use".
			if _, err := adb.Devices(ctx); err != nil {
				log.Warn("adb devices failed", "err", err)
			}
			sc := &provisioner.Scanner{ADB: adb, Targets: scanTargets, Log: log}
			go sc.Run(ctx, *scanInterval)
			log.Info("scanning for network adb devices", "targets", len(scanTargets), "interval", *scanInterval)
		}
		log.Info("watching for devices", "only", serials)
		_ = p.Watch(ctx, 3*time.Second, provisioner.WatchOptions{
			Include:  provisioner.WatchInclude(allowed, provisioner.IsScanTarget(scanTargets)),
			After:    afterProvision,
			Reporter: reporter,
		})
	case "slim", "unslim":
		failed := 0
		for _, s := range resolveTargets(ctx, log, adb, *all, serials) {
			var err error
			if cmd == "slim" {
				err = p.Slim(ctx, s, *slimTelephony)
			} else {
				err = p.Unslim(ctx, s)
			}
			if err != nil {
				log.Error(cmd+" failed", "serial", s, "err", err)
				failed++
			}
		}
		if failed > 0 {
			os.Exit(1)
		}
	case "heal":
		if len(serials) == 0 {
			fail(log, fmt.Errorf("-serial required"))
		}
		for _, s := range dedupe(serials) {
			fixed, err := p.Heal(ctx, s)
			if errors.Is(err, provisioner.ErrAgentNotRunning) {
				log.Error("agent not running on the device; run 'pcprov provision' to start it", "serial", s)
				os.Exit(1)
			}
			fail(log, err)
			if len(fixed) == 0 {
				log.Info("adb links ok", "serial", s)
			}
		}
	case "status", "stop":
		if len(serials) == 0 {
			fail(log, fmt.Errorf("-serial required"))
		}
		for _, s := range dedupe(serials) {
			if cmd == "stop" {
				fail(log, p.Stop(ctx, s))
				log.Info("agent stopped", "serial", s)
				continue
			}
			out, err := p.Status(ctx, s)
			fail(log, err)
			fmt.Printf("== %s\n%s\n", s, out)
		}
	default:
		usage()
	}
}

// resolveTargets returns the serials a one-shot subcommand should act on:
// -serial/-connect as given, plus every ready device when all is set.
func resolveTargets(ctx context.Context, log *slog.Logger, adb provisioner.ADB, all bool, serials multi) []string {
	if all {
		devs, err := adb.Devices(ctx)
		fail(log, err)
		for _, d := range devs {
			if d.State == "device" {
				serials = append(serials, d.Serial)
			}
		}
	}
	if len(serials) == 0 {
		fail(log, fmt.Errorf("no target: pass -serial, -connect or -all"))
	}
	return dedupe(serials)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// controllerToken reads the first line of path that is not blank or a "#"
// comment (mirrors the controller's own -admin-token-file format).
func controllerToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			return line, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s: no admin token", path)
}

func fail(log *slog.Logger, err error) {
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}
