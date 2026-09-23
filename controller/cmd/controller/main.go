// Command controller runs the PhoneBorg control plane (milestone 1:
// registration, inventory, benchmarks, heartbeats, metrics, dashboard).
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dzaczek/phoneborg/controller"
	"github.com/dzaczek/phoneborg/controller/gateway"
)

const (
	usageFlushInterval = 30 * time.Second
	shutdownGrace      = 10 * time.Second // in-flight requests get this long on SIGINT/SIGTERM
)

func main() {
	addr := flag.String("listen", ":18080", "HTTP listen address")
	hb := flag.Duration("heartbeat-interval", 5*time.Second, "heartbeat interval requested from nodes")
	suspect := flag.Int("suspect-after-missed", 3, "missed heartbeats before a node is SUSPECT")
	offline := flag.Int("offline-after-missed", 6, "missed heartbeats before a node is OFFLINE")
	debug := flag.Bool("debug", false, "debug logging (logs every heartbeat)")
	backendHost := flag.String("backend-host", "127.0.0.1", "host where nodes' advertised ports (adb forwards) are reachable")
	keysFile := flag.String("api-keys-file", "", "API keys file (\"<name> sha256:<hex>\" or legacy \"<name> <key>\" lines), reloaded on SIGHUP; empty = no auth (dev only)")
	adminTokenFile := flag.String("admin-token-file", "", "file with the admin API bearer token; empty = admin API disabled")
	stateDir := flag.String("state-dir", "", "directory for persistent state (usage.json); empty = keep usage in memory only")
	upstreamTimeout := flag.Duration("upstream-timeout", 120*time.Second, "max duration of one proxied inference request")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	fatal := func(msg string, args ...any) {
		log.Error(msg, args...)
		os.Exit(1)
	}

	keys := gateway.NewStaticKeys("")
	if *keysFile != "" {
		var err error
		if keys, err = gateway.LoadKeysFile(*keysFile); err != nil {
			fatal("loading API keys", "err", err)
		}
		warnIfShared(log, *keysFile)
		if len(keys.List()) == 0 {
			log.Warn("API keys file has no keys; every gateway request is rejected until one is created", "file", *keysFile)
		}
	} else {
		log.Warn("API authentication disabled; set -api-keys-file outside development")
	}

	var admin controller.AdminOptions
	if *adminTokenFile != "" {
		tok, err := controller.LoadAdminToken(*adminTokenFile)
		if err != nil {
			fatal("loading admin token", "err", err)
		}
		warnIfShared(log, *adminTokenFile)
		admin.Token = tok
	} else {
		log.Info("admin API disabled; set -admin-token-file to enable it")
	}

	usage, err := controller.NewUsage(*stateDir)
	if err != nil {
		fatal("loading usage state", "err", err)
	}

	reg := controller.NewRegistry(time.Duration(*suspect)**hb, time.Duration(*offline)**hb, log)
	srv := controller.NewServer(reg, *hb, controller.GatewayOptions{
		Keys:        keys,
		Usage:       usage,
		BackendHost: *backendHost,
		Config:      gateway.Config{UpstreamTimeout: *upstreamTimeout, MaxAttempts: 2, Cooldown: 30 * time.Second},
	}, admin, log)

	go func() {
		for range time.Tick(time.Second) {
			reg.Sweep()
			srv.ReapGateway()
		}
	}()
	go func() {
		for range time.Tick(usageFlushInterval) {
			if err := usage.Flush(); err != nil {
				log.Error("saving usage", "err", err)
			}
		}
	}()
	go func() {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		for range hup {
			if err := keys.Reload(); err != nil {
				log.Error("reloading API keys; keeping the current ones", "err", err)
				continue
			}
			log.Info("API keys reloaded", "file", keys.Path(), "keys", len(keys.List()))
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hs := &http.Server{Addr: *addr, Handler: srv.Handler()}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("controller listening", "addr", *addr, "heartbeat_interval", hb.String(),
		"admin_api", admin.Token != "", "state_dir", *stateDir)

	select {
	case err := <-errc:
		fatal("listen failed", "err", err)
	case <-ctx.Done():
	}
	log.Info("shutting down", "grace", shutdownGrace.String())
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Error("shutdown", "err", err)
	}
	_ = hs.Close() // cut requests still running after the grace period
	if err := usage.Flush(); err != nil {
		log.Error("saving usage", "err", err)
	}
	log.Info("stopped")
}

// warnIfShared warns when a secrets file is readable by group or others.
func warnIfShared(log *slog.Logger, path string) {
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		log.Warn("secrets file is readable by other users; chmod 600 it", "file", path, "mode", st.Mode().Perm().String())
	}
}
