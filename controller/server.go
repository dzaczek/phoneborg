package controller

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/controller/ui"
	"github.com/dzaczek/phoneborg/proto"
)

type Server struct {
	reg               *Registry
	log               *slog.Logger
	heartbeatInterval time.Duration
	registrations     prometheus.Counter
	heartbeats        prometheus.Counter
	benchmarks        prometheus.Counter
	transitions       *prometheus.CounterVec
	promReg           *prometheus.Registry
	gw                *gateway.Gateway
	keys              *gateway.StaticKeys
	usage             *Usage
	affinity          *gateway.Affinity
	least             *gateway.LeastInflight
	spread            *gateway.Spread // pools with routing "spread" (ADR-014)
	pools             *pools

	settingsMu sync.Mutex // serialises gateway settings changes
	policy     string

	// thermalLimitC holds a float64 (math.Float64bits), read on every routing
	// decision; 0 disables thermal-aware routing (ADR-010).
	thermalLimitC atomic.Uint64

	adminEnabled   bool
	adminTokenHash [sha256.Size]byte
	mAdmin         *prometheus.CounterVec

	catalog *models.Catalog
	place   *placement
}

// GatewayOptions configures the inference proxy and model management.
type GatewayOptions struct {
	// Keys authenticates gateway requests. nil means an open, in-memory store:
	// every request is served, known keys are attributed to their owner.
	Keys        *gateway.StaticKeys
	Usage       *Usage // nil = in-memory usage since start
	BackendHost string // host that reaches nodes' advertised ports (adb forward runs there)
	// ThermalLimitC marks a node hot when its last heartbeat temperature is
	// at or above this; 0 disables thermal-aware routing (ADR-010).
	ThermalLimitC float64
	Models        ModelOptions   // ADR-011
	Routing       RoutingOptions // node aliases and pools, ADR-014
	gateway.Config
}

func NewServer(reg *Registry, heartbeatInterval time.Duration, gwOpts GatewayOptions, admin AdminOptions, log *slog.Logger) *Server {
	if gwOpts.Keys == nil {
		gwOpts.Keys = gateway.NewStaticKeys("")
	}
	if gwOpts.Usage == nil {
		gwOpts.Usage, _ = NewUsage("") // cannot fail without a state dir
	}
	gwOpts.Config.Usage = gwOpts.Usage
	if gwOpts.Models.Catalog == nil {
		gwOpts.Models.Catalog, _ = models.NewCatalog(models.CatalogOptions{Log: log}) // cannot fail without a state file
	}
	if gwOpts.Models.Planner == nil {
		gwOpts.Models.Planner = models.DefaultPlanner{}
	}
	s := &Server{
		catalog: gwOpts.Models.Catalog,
		place: &placement{planner: gwOpts.Models.Planner, path: gwOpts.Models.PlacementFile,
			spec: gwOpts.Models.Placement, byNode: map[string]models.Assignment{}},
		keys:           gwOpts.Keys,
		usage:          gwOpts.Usage,
		least:          &gateway.LeastInflight{},
		spread:         &gateway.Spread{},
		pools:          &pools{path: gwOpts.Routing.File, byName: map[string]Pool{}},
		policy:         PolicyAffinity,
		adminEnabled:   admin.Token != "",
		adminTokenHash: sha256.Sum256([]byte(admin.Token)),
		mAdmin: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "phoneborg_admin_actions_total", Help: "Admin API calls by action and result (ok, error, unauthorized)."},
			[]string{"action", "result"}),
		reg:               reg,
		log:               log,
		heartbeatInterval: heartbeatInterval,
		registrations: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "phoneborg_controller_registrations_total", Help: "Node registrations accepted."}),
		heartbeats: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "phoneborg_controller_heartbeats_total", Help: "Heartbeats accepted."}),
		benchmarks: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "phoneborg_controller_benchmarks_total", Help: "Benchmark reports accepted."}),
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "phoneborg_node_state_transitions_total", Help: "Node state transitions."}, []string{"from", "to"}),
		promReg: prometheus.NewRegistry(),
	}
	s.thermalLimitC.Store(math.Float64bits(gwOpts.ThermalLimitC))
	if len(gwOpts.Routing.State.Aliases) > 0 {
		reg.LoadAliases(gwOpts.Routing.State.Aliases)
	}
	for _, p := range gwOpts.Routing.State.Pools {
		s.pools.byName[p.Name] = p
	}
	reg.onTransition = func(_ string, from, to proto.NodeState) {
		s.transitions.WithLabelValues(string(from), string(to)).Inc()
	}
	s.promReg.MustRegister(s.registrations, s.heartbeats, s.benchmarks, s.transitions, s.mAdmin,
		&nodeCollector{reg: reg, thermalLimitC: s.ThermalLimitC}, &modelCollector{s: s}, &poolCollector{s: s}, prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	s.affinity = gateway.NewAffinity(s.promReg)
	s.gw = gateway.New(gwOpts.Keys, s.affinity, func() []gateway.Backend {
		nodes, drained := reg.View()
		return Backends(nodes, drained, gwOpts.BackendHost, s.ThermalLimitC())
	}, gwOpts.Config, s.promReg, log)
	s.gw.SetTargets(serverTargets{s})
	s.catalog.SetOnChange(s.Replan)
	s.Replan()
	return s
}

// ThermalLimitC returns the current thermal-routing limit in Celsius; 0
// disables it (ADR-010).
func (s *Server) ThermalLimitC() float64 { return math.Float64frombits(s.thermalLimitC.Load()) }

// SetThermalLimitC changes the thermal-routing limit at runtime (PUT
// /admin/gateway, ADR-010).
func (s *Server) SetThermalLimitC(c float64) { s.thermalLimitC.Store(math.Float64bits(c)) }

// Backends lists nodes that can take inference traffic: ACTIVE with a ready
// runtime that advertises a port and is not switching models (ADR-011).
// Drained nodes are included but marked, so their in-flight requests are not
// reaped.
//
// Speed favors measured generation speed (RuntimeStatus.GenTPS, real
// llama.cpp tokens/s from the node's self-test) over the synthetic startup
// benchmark (Benchmark.CPUGFLOPS, an arbitrary score from a Go microbenchmark
// with no fixed relationship to tokens/s): the two units are not comparable,
// so once any candidate reports a measured speed, nodes that have not yet
// measured theirs get Speed 0 rather than being compared on the wrong scale
// (ADR-010).
//
// thermalLimitC marks a node Hot when its last heartbeat temperature is at or
// above it; 0 disables thermal marking.
func Backends(nodes []proto.Node, drained map[string]bool, defaultHost string, thermalLimitC float64) []gateway.Backend {
	var out []gateway.Backend
	var measured []bool
	anyMeasured := false
	for _, n := range nodes {
		hb := n.LastHeartbeat
		if n.State != proto.StateActive || hb == nil || hb.Runtime == nil || !hb.Runtime.Ready || hb.Runtime.AdvertisePort == 0 ||
			switching(hb.Runtime) {
			continue
		}
		host := hb.Runtime.AdvertiseHost
		if host == "" {
			host = defaultHost
		}
		b := gateway.Backend{
			NodeID:  n.ID,
			Model:   hb.Runtime.Model,
			Drained: drained[n.ID],
			URL:     fmt.Sprintf("http://%s:%d", host, hb.Runtime.AdvertisePort),
			Hot:     thermalLimitC > 0 && hb.TemperatureC != nil && *hb.TemperatureC >= thermalLimitC,
			CtxSize: hb.Runtime.CtxSize,
			Alias:   n.Alias,
		}
		gotMeasured := hb.Runtime.GenTPS > 0
		if gotMeasured {
			b.Speed = hb.Runtime.GenTPS
			anyMeasured = true
		} else if n.Benchmark != nil {
			b.Speed = n.Benchmark.CPUGFLOPS
		}
		out = append(out, b)
		measured = append(measured, gotMeasured)
	}
	if anyMeasured {
		for i := range out {
			if !measured[i] {
				out[i].Speed = 0
			}
		}
	}
	return out
}

// switching reports whether a node is downloading or loading a model. An
// agent reports Ready=false then; this also holds if it does not.
func switching(rt *proto.RuntimeStatus) bool {
	return rt.State == "downloading" || rt.State == "loading"
}

// ReapGateway cancels requests stuck on nodes that left the ready set.
func (s *Server) ReapGateway() { s.gw.Reap() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+proto.PathRegister, s.handleRegister)
	mux.HandleFunc("POST "+proto.PathBenchmark, s.handleBenchmark)
	mux.HandleFunc("POST "+proto.PathHeartbeat, s.handleHeartbeat)
	mux.HandleFunc("GET "+proto.PathNodes, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.reg.Snapshot())
	})
	mux.HandleFunc("GET "+PathModelFiles+"{model_id}", s.handleModelFile)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.promReg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	mux.Handle("GET /ui/", ui.Handler())
	mux.HandleFunc("GET /status", s.handleDashboard) // the plain read-only table, no login
	s.gw.Register(mux)
	s.registerAdmin(mux)
	return mux
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req proto.RegisterRequest
	if !decode(w, r, &req) {
		return
	}
	if req.NodeID == "" {
		httpError(w, http.StatusBadRequest, "node_id is required")
		return
	}
	s.reg.Register(req, r.RemoteAddr)
	s.registrations.Inc()
	s.Replan()
	s.log.Info("node registered", "node_id", req.NodeID, "model", req.Inventory.Model,
		"abi", req.Inventory.ABI, "cores", req.Inventory.CPUCores, "ram_bytes", req.Inventory.RAMTotalBytes)
	writeJSON(w, http.StatusOK, proto.RegisterResponse{
		NodeID: req.NodeID, HeartbeatIntervalSec: int(s.heartbeatInterval / time.Second)})
}

func (s *Server) handleBenchmark(w http.ResponseWriter, r *http.Request) {
	var rep proto.BenchmarkReport
	if !decode(w, r, &rep) {
		return
	}
	if err := s.reg.ReportBenchmark(rep); err != nil {
		notFoundOr500(w, err)
		return
	}
	s.benchmarks.Inc()
	s.log.Info("node benchmark", "node_id", rep.NodeID, "kind", rep.Benchmark.Kind,
		"cpu_gflops", rep.Benchmark.CPUGFLOPS, "mem_gbps", rep.Benchmark.MemBandwidthGBps)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var hb proto.Heartbeat
	if !decode(w, r, &hb) {
		return
	}
	if err := s.reg.Heartbeat(hb); err != nil {
		notFoundOr500(w, err)
		return
	}
	s.heartbeats.Inc()
	s.log.Debug("heartbeat", "node_id", hb.NodeID, "ram_avail", hb.RAMAvailBytes, "load1", hb.Load1)
	d := s.desired(hb.NodeID)
	if d == nil {
		w.WriteHeader(http.StatusNoContent) // keep the current runtime
		return
	}
	if hb.Runtime == nil || currentModel(hb.Runtime) != d.ModelID {
		s.log.Debug("desired runtime sent", "node_id", hb.NodeID, "model_id", d.ModelID, "current_model", currentModel(hb.Runtime))
	}
	writeJSON(w, http.StatusOK, proto.HeartbeatResponse{Desired: d})
}

// handleModelFile serves a ready catalog model to agents, with Range
// support so they can resume. Like the heartbeat, it needs no token
// (ADR-011).
func (s *Server) handleModelFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("model_id")
	path, m, ok := s.catalog.File(id)
	if !ok {
		httpError(w, http.StatusNotFound, "no ready model "+id)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.log.Error("opening model file", "model_id", id, "err", err)
		httpError(w, http.StatusNotFound, "model file unavailable")
		return
	}
	defer f.Close()
	s.log.Info("model file requested", "model_id", id, "range", r.Header.Get("Range"), "remote_addr", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+m.SHA256+`"`)
	http.ServeContent(w, r, m.File, time.Time{}, f)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func notFoundOr500(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUnknownNode) {
		httpError(w, http.StatusNotFound, "unknown node, re-register")
		return
	}
	httpError(w, http.StatusInternalServerError, err.Error())
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// nodeCollector exports per-node gauges from a registry snapshot at scrape
// time, so removed/renamed nodes never leave stale series behind.
type nodeCollector struct {
	reg *Registry
	// thermalLimitC returns the current thermal-routing limit (ADR-010).
	thermalLimitC func() float64
}

var (
	descNodes            = prometheus.NewDesc("phoneborg_nodes", "Nodes by state.", []string{"state"}, nil)
	descUp               = prometheus.NewDesc("phoneborg_node_up", "1 if node is ACTIVE.", []string{"node_id", "model"}, nil)
	descRAMTotal         = prometheus.NewDesc("phoneborg_node_ram_total_bytes", "Usable RAM.", []string{"node_id"}, nil)
	descRAMAvail         = prometheus.NewDesc("phoneborg_node_ram_available_bytes", "Available RAM at last heartbeat.", []string{"node_id"}, nil)
	descCores            = prometheus.NewDesc("phoneborg_node_cpu_cores", "Effective CPU cores.", []string{"node_id"}, nil)
	descGFLOPS           = prometheus.NewDesc("phoneborg_node_benchmark_cpu_gflops", "Benchmark CPU score.", []string{"node_id", "kind"}, nil)
	descMemBW            = prometheus.NewDesc("phoneborg_node_benchmark_mem_bandwidth_gbps", "Benchmark memory bandwidth.", []string{"node_id", "kind"}, nil)
	descLoad             = prometheus.NewDesc("phoneborg_node_load1", "1-minute load average.", []string{"node_id"}, nil)
	descTemp             = prometheus.NewDesc("phoneborg_node_temperature_celsius", "Hottest readable sensor.", []string{"node_id"}, nil)
	descBattery          = prometheus.NewDesc("phoneborg_node_battery_level_percent", "Battery level.", []string{"node_id"}, nil)
	descRuntimeReady     = prometheus.NewDesc("phoneborg_node_runtime_ready", "1 if the node's inference server is ready.", []string{"node_id", "model"}, nil)
	descRuntimeRestarts  = prometheus.NewDesc("phoneborg_node_runtime_restarts", "Inference server restarts since agent start.", []string{"node_id"}, nil)
	descRuntimeThreads   = prometheus.NewDesc("phoneborg_node_runtime_threads", "Inference threads, labelled with the engine build.", []string{"node_id", "engine"}, nil)
	descRuntimeGenTPS    = prometheus.NewDesc("phoneborg_node_runtime_gen_tokens_per_second", "Generation speed from the node's self-test against its own llama-server (ADR-010). Not to be confused with phoneborg_node_generation_tokens_per_second, the gateway's last proxied request.", []string{"node_id"}, nil)
	descRuntimePromptTPS = prometheus.NewDesc("phoneborg_node_runtime_prompt_tokens_per_second", "Prompt processing speed from the node's self-test against its own llama-server (ADR-010).", []string{"node_id"}, nil)
	descHeartbeatAge     = prometheus.NewDesc("phoneborg_node_last_seen_age_seconds", "Seconds since last message from node.", []string{"node_id"}, nil)
	descDrained          = prometheus.NewDesc("phoneborg_node_drained", "1 if the node is drained (no new inference requests).", []string{"node_id"}, nil)
	descHot              = prometheus.NewDesc("phoneborg_node_hot", "1 if the node's last heartbeat temperature is at or above -thermal-limit-c.", []string{"node_id"}, nil)
)

func (c *nodeCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descNodes, descUp, descRAMTotal, descRAMAvail, descCores, descGFLOPS, descMemBW, descLoad, descTemp, descBattery, descHeartbeatAge, descRuntimeReady, descRuntimeRestarts, descRuntimeThreads, descRuntimeGenTPS, descRuntimePromptTPS, descDrained, descHot} {
		ch <- d
	}
}

func (c *nodeCollector) Collect(ch chan<- prometheus.Metric) {
	nodes, drained := c.reg.View()
	counts := map[proto.NodeState]int{}
	g := prometheus.GaugeValue
	for _, n := range nodes {
		counts[n.State]++
		up := 0.0
		if n.State == proto.StateActive {
			up = 1
		}
		ch <- prometheus.MustNewConstMetric(descUp, g, up, n.ID, n.Inventory.Model)
		ch <- prometheus.MustNewConstMetric(descRAMTotal, g, float64(n.Inventory.RAMTotalBytes), n.ID)
		ch <- prometheus.MustNewConstMetric(descCores, g, float64(n.Inventory.CPUCores), n.ID)
		ch <- prometheus.MustNewConstMetric(descHeartbeatAge, g, time.Since(n.LastSeen).Seconds(), n.ID)
		d := 0.0
		if drained[n.ID] {
			d = 1
		}
		ch <- prometheus.MustNewConstMetric(descDrained, g, d, n.ID)
		if b := n.Benchmark; b != nil {
			ch <- prometheus.MustNewConstMetric(descGFLOPS, g, b.CPUGFLOPS, n.ID, b.Kind)
			ch <- prometheus.MustNewConstMetric(descMemBW, g, b.MemBandwidthGBps, n.ID, b.Kind)
		}
		if hb := n.LastHeartbeat; hb != nil {
			ch <- prometheus.MustNewConstMetric(descRAMAvail, g, float64(hb.RAMAvailBytes), n.ID)
			ch <- prometheus.MustNewConstMetric(descLoad, g, hb.Load1, n.ID)
			hot := 0.0
			if limit := c.thermalLimitC(); limit > 0 && hb.TemperatureC != nil && *hb.TemperatureC >= limit {
				hot = 1
			}
			ch <- prometheus.MustNewConstMetric(descHot, g, hot, n.ID)
			if hb.TemperatureC != nil {
				ch <- prometheus.MustNewConstMetric(descTemp, g, *hb.TemperatureC, n.ID)
			}
			if hb.BatteryLevel != nil {
				ch <- prometheus.MustNewConstMetric(descBattery, g, float64(*hb.BatteryLevel), n.ID)
			}
			if rt := hb.Runtime; rt != nil {
				ready := 0.0
				if rt.Ready {
					ready = 1
				}
				ch <- prometheus.MustNewConstMetric(descRuntimeReady, g, ready, n.ID, rt.Model)
				ch <- prometheus.MustNewConstMetric(descRuntimeRestarts, g, float64(rt.Restarts), n.ID)
				ch <- prometheus.MustNewConstMetric(descRuntimeThreads, g, float64(rt.Threads), n.ID, rt.Engine)
				if rt.GenTPS > 0 {
					ch <- prometheus.MustNewConstMetric(descRuntimeGenTPS, g, rt.GenTPS, n.ID)
				}
				if rt.PromptTPS > 0 {
					ch <- prometheus.MustNewConstMetric(descRuntimePromptTPS, g, rt.PromptTPS, n.ID)
				}
			}
		}
	}
	for _, st := range proto.AllStates {
		ch <- prometheus.MustNewConstMetric(descNodes, g, float64(counts[st]), string(st))
	}
}

var dashboardTmpl = template.Must(template.New("d").Funcs(template.FuncMap{
	"gib": func(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) },
	"ago": func(t time.Time) string { return time.Since(t).Round(time.Second).String() },
	"temp": func(p *float64) string {
		if p == nil {
			return "n/a"
		}
		return fmt.Sprintf("%.1f °C", *p)
	},
}).Parse(`<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="3">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>PhoneBorg</title>
<style>body{font:14px system-ui,sans-serif;margin:16px;background:#fafafa;color:#222}
table{border-collapse:collapse;width:100%}td,th{padding:6px 8px;border-bottom:1px solid #ddd;text-align:left}
.ACTIVE{color:#1a7f37}.DRAINED{color:#805ad5}.SUSPECT{color:#b7791f}.OFFLINE{color:#c53030}.BENCHMARKING{color:#2b6cb0}
@media(prefers-color-scheme:dark){body{background:#161616;color:#ddd}td,th{border-color:#333}}</style></head><body>
<h2>PhoneBorg — {{len .}} node(s)</h2>
<table><tr><th>Node</th><th>State</th><th>Device</th><th>Android</th><th>Cores</th><th>RAM avail / total</th>
<th>CPU GFLOPS</th><th>Mem GB/s</th><th>Load</th><th>Temp</th><th>Model</th><th>Last seen</th></tr>
{{range .}}<tr><td>{{.ID}}</td><td class="{{.State}}"><b>{{.State}}</b>{{if .Drained}} <b class="DRAINED">DRAINED</b>{{end}}</td>
<td>{{.Inventory.Manufacturer}} {{.Inventory.Model}}<br><small>{{.Inventory.SoC}} {{.Inventory.ABI}}</small></td>
<td>{{.Inventory.AndroidRelease}} (SDK {{.Inventory.SDK}})</td><td>{{.Inventory.CPUCores}}</td>
<td>{{with .LastHeartbeat}}{{gib .RAMAvailBytes}}{{else}}-{{end}} / {{gib .Inventory.RAMTotalBytes}}</td>
<td>{{with .Benchmark}}{{printf "%.1f" .CPUGFLOPS}}{{else}}…{{end}}</td>
<td>{{with .Benchmark}}{{printf "%.1f" .MemBandwidthGBps}}{{else}}…{{end}}</td>
<td>{{with .LastHeartbeat}}{{printf "%.2f" .Load1}}{{end}}</td>
<td>{{with .LastHeartbeat}}{{temp .TemperatureC}}{{end}}</td>
<td>{{with .LastHeartbeat}}{{with .Runtime}}{{.Model}} {{if .Ready}}<span class="ACTIVE">ready</span>{{else}}<span class="SUSPECT">loading</span>{{end}}{{end}}{{end}}</td>
<td>{{ago .LastSeen}} ago</td></tr>
{{else}}<tr><td colspan="12">No nodes yet. Connect a phone and run <code>pcprov watch</code>.</td></tr>{{end}}
</table><p><a href="/ui/">management panel</a> · <a href="/metrics">/metrics</a> · <a href="/v1/nodes">/v1/nodes</a> · <a href="/v1/models">/v1/models</a></p></body></html>`))

type dashboardRow struct {
	proto.Node
	Drained bool
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	nodes, drained := s.reg.View()
	rows := make([]dashboardRow, len(nodes))
	for i, n := range nodes {
		rows[i] = dashboardRow{Node: n, Drained: drained[n.ID]}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTmpl.Execute(w, rows); err != nil {
		s.log.Error("dashboard render", "err", err)
	}
}
