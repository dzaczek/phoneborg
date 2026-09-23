// Command controller runs the PhoneBorg control plane (milestone 1:
// registration, inventory, benchmarks, heartbeats, metrics, dashboard).
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/dzaczek/phoneborg/controller"
	"github.com/dzaczek/phoneborg/controller/gateway"
)

func main() {
	addr := flag.String("listen", ":18080", "HTTP listen address")
	hb := flag.Duration("heartbeat-interval", 5*time.Second, "heartbeat interval requested from nodes")
	suspect := flag.Int("suspect-after-missed", 3, "missed heartbeats before a node is SUSPECT")
	offline := flag.Int("offline-after-missed", 6, "missed heartbeats before a node is OFFLINE")
	debug := flag.Bool("debug", false, "debug logging (logs every heartbeat)")
	backendHost := flag.String("backend-host", "127.0.0.1", "host where nodes' advertised ports (adb forwards) are reachable")
	keysFile := flag.String("api-keys-file", "", "file with \"<name> <key>\" lines; empty = no auth (dev only)")
	upstreamTimeout := flag.Duration("upstream-timeout", 120*time.Second, "max duration of one proxied inference request")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	var auth gateway.Authenticator = gateway.AllowAll{}
	if *keysFile != "" {
		keys, err := gateway.LoadKeysFile(*keysFile)
		if err != nil {
			log.Error("loading API keys", "err", err)
			os.Exit(1)
		}
		auth = keys
	} else {
		log.Warn("API authentication disabled; set -api-keys-file outside development")
	}

	reg := controller.NewRegistry(time.Duration(*suspect)**hb, time.Duration(*offline)**hb, log)
	srv := controller.NewServer(reg, *hb, controller.GatewayOptions{
		Auth:        auth,
		BackendHost: *backendHost,
		Config:      gateway.Config{UpstreamTimeout: *upstreamTimeout, MaxAttempts: 2, Cooldown: 30 * time.Second},
	}, log)

	go func() {
		for range time.Tick(time.Second) {
			reg.Sweep()
			srv.ReapGateway()
		}
	}()

	log.Info("controller listening", "addr", *addr, "heartbeat_interval", hb.String())
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Error("listen failed", "err", err)
		os.Exit(1)
	}
}
