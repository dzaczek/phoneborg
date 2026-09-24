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
	"path/filepath"
	"syscall"
	"time"

	"github.com/dzaczek/phoneborg/controller"
	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/controller/models"
)

const (
	usageFlushInterval = 30 * time.Second
	replanInterval     = 10 * time.Second // placement is also re-planned on every change (ADR-011)
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
	stateDir := flag.String("state-dir", "", "directory for persistent state (usage.json, models.json, placement.json, routing.json); empty = keep it in memory only")
	modelsDir := flag.String("models-dir", "", "directory for model files served to nodes; default <state-dir>/models, or a temporary directory without -state-dir")
	upstreamTimeout := flag.Duration("upstream-timeout", 120*time.Second, "max duration of one proxied inference request")
	thermalLimit := flag.Float64("thermal-limit-c", 75, "temperature (Celsius) at or above which a node is \"hot\" and gets no new sessions; 0 disables thermal-aware routing")
	minPredictedTokS := flag.Float64("min-predicted-tok-s", 3, "minimum predicted generation tok/s (ADR-015) below which the planner will not place a model on a node; a policy's own min_tok_s overrides it; unknown predictions never exclude")
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

	modelOpts, err := loadModels(*stateDir, *modelsDir, log)
	if err != nil {
		fatal("loading model catalog", "err", err)
	}
	defer modelOpts.Catalog.Close()
	modelOpts.MinPredictedTokS = *minPredictedTokS

	var routing controller.RoutingOptions
	if *stateDir != "" {
		routing.File = filepath.Join(*stateDir, "routing.json") // node aliases and pools (ADR-014)
		if routing.State, err = controller.LoadRouting(routing.File); err != nil {
			fatal("loading node aliases and pools", "err", err)
		}
	}

	reg := controller.NewRegistry(time.Duration(*suspect)**hb, time.Duration(*offline)**hb, log)
	srv := controller.NewServer(reg, *hb, controller.GatewayOptions{
		Keys:          keys,
		Usage:         usage,
		BackendHost:   *backendHost,
		ThermalLimitC: *thermalLimit,
		Models:        modelOpts,
		Routing:       routing,
		Config:        gateway.Config{UpstreamTimeout: *upstreamTimeout, MaxAttempts: 2, Cooldown: 30 * time.Second},
	}, admin, log)

	go func() {
		for range time.Tick(time.Second) {
			reg.Sweep()
			srv.ReapGateway()
		}
	}()
	go func() {
		for range time.Tick(replanInterval) {
			srv.Replan()
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
		"admin_api", admin.Token != "", "state_dir", *stateDir, "models_dir", modelOpts.Catalog.Dir())

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

// loadModels opens the model catalog and placement (ADR-011). Without a
// state directory both live in memory, and model files go to -models-dir or
// a temporary directory created on the first download.
func loadModels(stateDir, modelsDir string, log *slog.Logger) (controller.ModelOptions, error) {
	opts := models.CatalogOptions{Dir: modelsDir, Log: log}
	var o controller.ModelOptions
	if stateDir != "" {
		if opts.Dir == "" {
			opts.Dir = filepath.Join(stateDir, "models")
		}
		opts.StateFile = filepath.Join(stateDir, "models.json")
		o.PlacementFile = filepath.Join(stateDir, "placement.json")
		spec, err := controller.LoadPlacement(o.PlacementFile)
		if err != nil {
			return o, err
		}
		o.Placement = spec
	} else {
		log.Warn("no -state-dir: the model catalog and placement are kept in memory only")
	}
	c, err := models.NewCatalog(opts)
	o.Catalog = c
	return o, err
}

// warnIfShared warns when a secrets file is readable by group or others.
func warnIfShared(log *slog.Logger, path string) {
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		log.Warn("secrets file is readable by other users; chmod 600 it", "file", path, "mode", st.Mode().Perm().String())
	}
}
