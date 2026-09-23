// Command pcprov provisions Android phones into a PhoneBorg over ADB.
//
//	pcprov devices
//	pcprov provision -serial <serial> | -all
//	pcprov watch                 # auto-provision phones as they are plugged in
//	pcprov status -serial <serial>
//	pcprov stop   -serial <serial>
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
	fmt.Fprintln(os.Stderr, "usage: pcprov <devices|provision|watch|status|stop> [flags]\n  run 'pcprov <cmd> -h' for flags")
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
	llamaServer := fs.String("llama-server", "bin/llama/llama-server", "arm64 llama-server binary (make llama)")
	servePort := fs.Int("serve-port", 18090, "on-device llama-server port")
	all := fs.Bool("all", false, "provision all ready devices")
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
		Model: *model, LlamaServer: *llamaServer, ServePort: *servePort}}

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
			for _, f := range []string{*model, *llamaServer} {
				if _, err := os.Stat(f); err != nil {
					log.Error("serving file missing (make llama / download model)", "path", f)
					os.Exit(1)
				}
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
		if *all {
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
		failed := 0
		for _, s := range dedupe(serials) {
			if err := p.Provision(ctx, s); err != nil {
				log.Error("provision failed", "serial", s, "err", err)
				failed++
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
		log.Info("watching for devices", "only", serials)
		_ = p.Watch(ctx, 3*time.Second, func(s string) bool { return len(allowed) == 0 || allowed[s] })
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
