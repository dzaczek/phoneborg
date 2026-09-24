// Command pcprov provisions Android phones into a PhoneBorg over ADB.
//
//	pcprov devices
//	pcprov provision -serial <serial> | -all
//	pcprov watch                 # auto-provision phones as they are plugged in
//	pcprov status -serial <serial>
//	pcprov stop   -serial <serial>
//	pcprov slim   -serial <serial> | -all    # disable non-essential apps to free RAM
//	pcprov unslim -serial <serial> | -all    # restore what slim disabled
package main

import (
	"context"
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
	fmt.Fprintln(os.Stderr, "usage: pcprov <devices|provision|watch|status|stop|heal|slim|unslim> [flags]\n  run 'pcprov <cmd> -h' for flags")
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
	servePort := fs.Int("serve-port", 18090, "on-device llama-server port")
	all := fs.Bool("all", false, "provision all ready devices")
	slimFlag := fs.Bool("slim", false, "run slim after a successful provision (provision, watch)")
	slimTelephony := fs.Bool("slim-telephony", false, "slim/unslim: also disable dialer/contacts (off by default)")
	var serials, connects multi
	fs.Var(&serials, "serial", "device serial (repeatable); for watch, restricts to these serials")
	fs.Var(&connects, "connect", "adb connect host:port first (repeatable), e.g. emulated phones")
	_ = fs.Parse(os.Args[2:])

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	adb := provisioner.ADB{Bin: *adbBin, Timeout: 60 * time.Second}
	p := &provisioner.Provisioner{ADB: adb, Log: log, Opts: provisioner.Options{
		AgentBinary: *agent, ControllerPort: *port, AgentArgs: *agentArgs,
		Model: *model, LlamaServer: *llamaServer, LlamaDir: *llamaDir, ServePort: *servePort}}

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
		log.Info("watching for devices", "only", serials)
		_ = p.Watch(ctx, 3*time.Second, func(s string) bool { return len(allowed) == 0 || allowed[s] }, afterProvision)
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

func fail(log *slog.Logger, err error) {
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}
