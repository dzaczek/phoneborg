package controller

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/proto"
)

// Admin API (see docs/DECISIONS.md ADR-008). Every route needs
// `Authorization: Bearer <admin token>`. Without a token the API is disabled.

const adminDisabledMsg = "admin API disabled: start the controller with -admin-token-file"

// MinAdminTokenLen rejects tokens that are easy to guess.
const MinAdminTokenLen = 16

// Routing policy names accepted by PUT /admin/gateway.
const (
	PolicyAffinity      = "affinity"
	PolicyLeastInflight = "least_inflight"
)

// AdminOptions configures the admin API.
type AdminOptions struct {
	Token string // empty disables /admin/
}

// LoadAdminToken reads the admin token: the first line of path that is not
// blank or a # comment.
func LoadAdminToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) < MinAdminTokenLen {
			return "", fmt.Errorf("%s: admin token must be at least %d characters", path, MinAdminTokenLen)
		}
		return line, nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s: no admin token", path)
}

// AdminNode is a node as returned by GET /admin/nodes.
type AdminNode struct {
	proto.Node
	Drained        bool `json:"drained"`
	Inflight       int  `json:"inflight"`
	PinnedSessions int  `json:"pinned_sessions"`
	// Hot is true when the node's last heartbeat temperature is at or above
	// the current thermal limit (ADR-010); it then gets no new sessions.
	Hot bool `json:"hot"`
	// Class is the device class by total RAM (ADR-011). PerfTier, GenGBps
	// and PromptGBps are the node's measured performance class and memory
	// bandwidth (ADR-015); PerfTier is "?" and the GB/s fields are 0 when
	// not measured yet.
	Class      string  `json:"class"`
	PerfTier   string  `json:"perf_tier"`
	GenGBps    float64 `json:"gen_gbps,omitempty"`
	PromptGBps float64 `json:"prompt_gbps,omitempty"`
}

// AdminKey is one API key owner as returned by GET /admin/keys.
type AdminKey struct {
	Name    string        `json:"name"`
	Keys    int           `json:"keys"`             // keys owned by this name (legacy files may have several)
	Created time.Time     `json:"created,omitzero"` // zero for legacy entries
	Usage   UsageCounters `json:"usage"`
}

// AdminKeys is the response of GET /admin/keys.
type AdminKeys struct {
	AuthMode  string     `json:"auth_mode"` // "keys" or "open"
	Persisted bool       `json:"persisted"` // changes are written to -api-keys-file
	Keys      []AdminKey `json:"keys"`
}

// CreatedKey is the response of POST /admin/keys. Key is shown only once.
type CreatedKey struct {
	Name      string    `json:"name"`
	Key       string    `json:"key"`
	Created   time.Time `json:"created"`
	AuthMode  string    `json:"auth_mode"`
	Persisted bool      `json:"persisted"`
	Notes     []string  `json:"notes,omitempty"`
}

// GatewaySettings are the gateway's runtime settings (GET/PUT /admin/gateway).
type GatewaySettings struct {
	Policy          string  `json:"policy"`
	AffinitySpill   int     `json:"affinity_spill"`
	UpstreamTimeout string  `json:"upstream_timeout"`
	AuthMode        string  `json:"auth_mode"`
	ThermalLimitC   float64 `json:"thermal_limit_c"`
	// Access is the effective -gateway-access mode ("local", "keys" or
	// "open"): "keys" whenever key authentication is enforced, whichever way
	// it got enforced (ADR-017).
	Access string `json:"access"`
}

// GatewayUpdate is the body of PUT /admin/gateway; nil fields are unchanged.
type GatewayUpdate struct {
	Policy          *string  `json:"policy,omitempty"`
	AffinitySpill   *int     `json:"affinity_spill,omitempty"`
	UpstreamTimeout *string  `json:"upstream_timeout,omitempty"`
	AuthMode        *string  `json:"auth_mode,omitempty"`
	ThermalLimitC   *float64 `json:"thermal_limit_c,omitempty"`
}

// ClusterSummary is part of GET /admin/stats.
type ClusterSummary struct {
	Nodes         int                     `json:"nodes"`
	ByState       map[proto.NodeState]int `json:"by_state"`
	Drained       int                     `json:"drained"`
	ReadyBackends int                     `json:"ready_backends"` // ready, not drained
	Models        map[string]int          `json:"models"`         // ready, not drained nodes per model, external ones included
	External      int                     `json:"external"`       // ACTIVE external engine nodes (ADR-016), not counted in Nodes
	Inflight      int                     `json:"inflight"`
}

// Stats is the response of GET /admin/stats.
type Stats struct {
	StartedAt     time.Time      `json:"started_at"`
	UptimeSeconds float64        `json:"uptime_seconds"`
	Cluster       ClusterSummary `json:"cluster"`
	SinceStart    UsageTotals    `json:"since_start"`
	Lifetime      *UsageTotals   `json:"lifetime,omitempty"` // only with -state-dir
}

func (s *Server) registerAdmin(mux *http.ServeMux) {
	if !s.adminEnabled {
		mux.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) {
			httpError(w, http.StatusServiceUnavailable, adminDisabledMsg)
		})
		return
	}
	for _, rt := range []struct {
		pattern, action string
		fn              http.HandlerFunc
	}{
		{"GET /admin/nodes", "nodes_list", s.adminNodes},
		{"POST /admin/nodes/{id}/drain", "node_drain", s.adminDrain(true)},
		{"POST /admin/nodes/{id}/undrain", "node_undrain", s.adminDrain(false)},
		{"DELETE /admin/nodes/{id}", "node_forget", s.adminForget},
		{"GET /admin/keys", "keys_list", s.adminKeys},
		{"POST /admin/keys", "key_create", s.adminCreateKey},
		{"DELETE /admin/keys/{name}", "key_revoke", s.adminRevokeKey},
		{"GET /admin/stats", "stats", s.adminStats},
		{"GET /admin/gateway", "gateway_get", s.adminGateway},
		{"PUT /admin/gateway", "gateway_set", s.adminSetGateway},
	} {
		mux.Handle(rt.pattern, s.adminAuth(rt.action, rt.fn))
	}
	s.registerModelAdmin(func(pattern, action string, fn http.HandlerFunc) {
		mux.Handle(pattern, s.adminAuth(action, fn))
	})
	s.registerPoolAdmin(func(pattern, action string, fn http.HandlerFunc) {
		mux.Handle(pattern, s.adminAuth(action, fn))
	})
	s.registerExternalAdmin(func(pattern, action string, fn http.HandlerFunc) {
		mux.Handle(pattern, s.adminAuth(action, fn))
	})
	// Unknown admin paths also need the token, so they reveal nothing.
	mux.Handle("/admin/", s.adminAuth("unknown", http.NotFound))
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// adminAuth checks the admin token in constant time and counts the action.
func (s *Server) adminAuth(action string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		h := sha256.Sum256([]byte(strings.TrimSpace(tok)))
		if tok == "" || subtle.ConstantTimeCompare(h[:], s.adminTokenHash[:]) != 1 {
			s.mAdmin.WithLabelValues(action, "unauthorized").Inc()
			s.log.Warn("admin auth failed", "action", action, "remote_addr", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Bearer realm="phoneborg-admin"`)
			httpError(w, http.StatusUnauthorized, "invalid or missing admin token")
			return
		}
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next(sw, r)
		result := "ok"
		if sw.code >= 400 {
			result = "error"
		}
		s.mAdmin.WithLabelValues(action, result).Inc()
		if r.Method == http.MethodGet {
			s.log.Debug("admin read", "principal", "admin", "action", action, "status", sw.code)
		}
	})
}

// audit logs a state-changing admin action. Never pass secrets in attrs.
func (s *Server) audit(r *http.Request, action string, err error, attrs ...any) {
	attrs = append([]any{"principal", "admin", "action", action, "remote_addr", r.RemoteAddr}, attrs...)
	if err != nil {
		s.log.Warn("admin action failed", append(attrs, "err", err)...)
		return
	}
	s.log.Info("admin action", attrs...)
}

func (s *Server) adminNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.adminNodeList())
}

func (s *Server) adminNodeList() []AdminNode {
	nodes, drained := s.reg.View()
	inflight, pins := s.gw.Inflight(), s.affinity.Pins()
	limit := s.ThermalLimitC()
	out := make([]AdminNode, 0, len(nodes))
	for _, n := range nodes {
		hot := limit > 0 && n.LastHeartbeat != nil && n.LastHeartbeat.TemperatureC != nil && *n.LastHeartbeat.TemperatureC >= limit
		perf := s.perf.get(n.ID)
		out = append(out, AdminNode{Node: n, Drained: drained[n.ID], Inflight: inflight[n.ID], PinnedSessions: pins[n.ID], Hot: hot,
			Class: models.ClassOf(n.Inventory.RAMTotalBytes), PerfTier: models.PerfTierOf(perf.GenGBps), GenGBps: perf.GenGBps, PromptGBps: perf.PromptGBps})
	}
	return out
}

func (s *Server) adminDrain(drain bool) http.HandlerFunc {
	action := map[bool]string{true: "node_drain", false: "node_undrain"}[drain]
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var err error
		name, external := strings.CutPrefix(id, ExternalPrefix)
		if external { // external engine node (ADR-016)
			if err = s.ext.setDrained(name, drain); errors.Is(err, ErrExternalUnknown) {
				err = ErrUnknownNode
			}
		} else {
			err = s.reg.SetDrained(id, drain)
		}
		moved := 0
		if err == nil && drain {
			moved = s.affinity.Unpin(id) // pinned sessions move on their next request
		}
		if err == nil && !external {
			s.Replan() // drained nodes only follow pins (ADR-011)
		}
		s.audit(r, action, err, "node_id", id, "moved_sessions", moved)
		if err != nil {
			notFoundOr500(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"node_id": id, "drained": drain, "moved_sessions": moved})
	}
}

func (s *Server) adminForget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.reg.Forget(id)
	if err == nil {
		s.affinity.Unpin(id)
		s.Replan()
	}
	s.audit(r, "node_forget", err, "node_id", id)
	if err != nil {
		notFoundOr500(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authMode() string {
	if s.keys.Enforced() {
		return "keys"
	}
	return "open"
}

// accessMode reports the effective -gateway-access mode: "keys" whenever key
// authentication is enforced (startup flag or the runtime auth=keys switch),
// else the configured mode (ADR-017).
func (s *Server) accessMode() string {
	if s.keys.Enforced() {
		return gateway.AccessKeys
	}
	return s.accessModeCfg
}

// keyUsage prefers lifetime totals when they are persisted.
func (s *Server) keyUsage() map[string]UsageCounters {
	if t, ok := s.usage.Lifetime(); ok {
		return t.Keys
	}
	return s.usage.Session().Keys
}

func (s *Server) adminKeys(w http.ResponseWriter, r *http.Request) {
	usage := s.keyUsage()
	byName := map[string]*AdminKey{}
	var names []string
	for _, e := range s.keys.List() {
		k := byName[e.Name]
		if k == nil {
			k = &AdminKey{Name: e.Name, Usage: usage[e.Name]}
			byName[e.Name] = k
			names = append(names, e.Name)
		}
		k.Keys++
		if !e.Created.IsZero() && (k.Created.IsZero() || e.Created.Before(k.Created)) {
			k.Created = e.Created
		}
	}
	out := AdminKeys{AuthMode: s.authMode(), Persisted: s.keys.Path() != "", Keys: []AdminKey{}}
	for _, n := range names {
		out.Keys = append(out.Keys, *byName[n])
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminCreateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	key, e, err := s.keys.Create(req.Name)
	s.audit(r, "key_create", err, "key_name", req.Name)
	switch {
	case errors.Is(err, gateway.ErrKeyName):
		httpError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, gateway.ErrKeyExists):
		httpError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		httpError(w, http.StatusInternalServerError, "cannot store key: "+err.Error())
		return
	}
	out := CreatedKey{Name: e.Name, Key: key, Created: e.Created, AuthMode: s.authMode(), Persisted: s.keys.Path() != ""}
	if !out.Persisted {
		out.Notes = append(out.Notes, "no -api-keys-file: this key is kept in memory only and is lost on restart")
	}
	if !s.keys.Enforced() {
		out.Notes = append(out.Notes, `gateway is open: requests without a valid key are still served as "anonymous"; `+
			`enforce keys with PUT /admin/gateway {"auth_mode":"keys"} (pbctl gateway set auth=keys)`)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) adminRevokeKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.keys.Revoke(name)
	s.audit(r, "key_revoke", err, "key_name", name)
	switch {
	case errors.Is(err, gateway.ErrKeyUnknown):
		httpError(w, http.StatusNotFound, err.Error())
	case err != nil:
		httpError(w, http.StatusInternalServerError, "cannot store keys: "+err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) adminStats(w http.ResponseWriter, r *http.Request) {
	nodes, drained := s.reg.View()
	c := ClusterSummary{Nodes: len(nodes), ByState: map[proto.NodeState]int{}, Models: map[string]int{}}
	for _, st := range proto.AllStates {
		c.ByState[st] = 0
	}
	for _, n := range nodes {
		c.ByState[n.State]++
		if drained[n.ID] {
			c.Drained++
		}
	}
	for _, b := range Backends(nodes, drained, "", s.ThermalLimitC()) {
		if !b.Drained {
			c.ReadyBackends++
			c.Models[b.Model]++
		}
	}
	for _, b := range s.ext.backends() {
		if !b.Drained {
			c.Models[b.Model]++
		}
	}
	for _, x := range s.ext.list(nil, nil) {
		if x.State == ExternalActive {
			c.External++
		}
	}
	for _, n := range s.gw.Inflight() {
		c.Inflight += n
	}
	out := Stats{StartedAt: s.usage.Started(), UptimeSeconds: time.Since(s.usage.Started()).Seconds(),
		Cluster: c, SinceStart: s.usage.Session()}
	if t, ok := s.usage.Lifetime(); ok {
		out.Lifetime = &t
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) gatewaySettings() GatewaySettings {
	s.settingsMu.Lock()
	policy := s.policy
	s.settingsMu.Unlock()
	return GatewaySettings{Policy: policy, AffinitySpill: s.affinity.SpillThreshold(),
		UpstreamTimeout: s.gw.UpstreamTimeout().String(), AuthMode: s.authMode(), ThermalLimitC: s.ThermalLimitC(),
		Access: s.accessMode()}
}

func (s *Server) adminGateway(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.gatewaySettings())
}

func (s *Server) adminSetGateway(w http.ResponseWriter, r *http.Request) {
	var u GatewayUpdate
	if !decodeStrict(w, r, &u) {
		return
	}
	code, err := s.applyGatewayUpdate(u)
	attrs := []any{}
	if u.Policy != nil {
		attrs = append(attrs, "policy", *u.Policy)
	}
	if u.AffinitySpill != nil {
		attrs = append(attrs, "affinity_spill", *u.AffinitySpill)
	}
	if u.UpstreamTimeout != nil {
		attrs = append(attrs, "upstream_timeout", *u.UpstreamTimeout)
	}
	if u.AuthMode != nil {
		attrs = append(attrs, "auth_mode", *u.AuthMode)
	}
	if u.ThermalLimitC != nil {
		attrs = append(attrs, "thermal_limit_c", *u.ThermalLimitC)
	}
	s.audit(r, "gateway_set", err, attrs...)
	if err != nil {
		httpError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.gatewaySettings())
}

// applyGatewayUpdate validates the whole update, then applies it, so a bad
// field changes nothing. It returns an HTTP status for errors.
func (s *Server) applyGatewayUpdate(u GatewayUpdate) (int, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	var picker gateway.Picker
	if u.Policy != nil {
		switch *u.Policy {
		case PolicyAffinity:
			picker = s.affinity
		case PolicyLeastInflight:
			picker = s.least
		default:
			return http.StatusBadRequest, fmt.Errorf("policy must be %q or %q", PolicyAffinity, PolicyLeastInflight)
		}
	}
	if u.AffinitySpill != nil && (*u.AffinitySpill < 1 || *u.AffinitySpill > 1000) {
		return http.StatusBadRequest, errors.New("affinity_spill must be between 1 and 1000")
	}
	var timeout time.Duration
	if u.UpstreamTimeout != nil {
		d, err := time.ParseDuration(*u.UpstreamTimeout)
		if err != nil || d < time.Second || d > 24*time.Hour {
			return http.StatusBadRequest, errors.New("upstream_timeout must be a duration between 1s and 24h, e.g. \"600s\"")
		}
		timeout = d
	}
	if u.AuthMode != nil {
		switch *u.AuthMode {
		case "keys":
			if err := s.keys.Enforce(); err != nil {
				return http.StatusConflict, err
			}
		case "open":
			if s.keys.Enforced() {
				return http.StatusConflict, errors.New("key authentication cannot be disabled at runtime")
			}
		default:
			return http.StatusBadRequest, errors.New(`auth_mode must be "keys" or "open"`)
		}
	}
	if u.ThermalLimitC != nil && (*u.ThermalLimitC < 0 || *u.ThermalLimitC > 150) {
		return http.StatusBadRequest, errors.New("thermal_limit_c must be between 0 (disabled) and 150")
	}
	if picker != nil {
		s.gw.SetPicker(picker)
		s.policy = *u.Policy
	}
	if u.AffinitySpill != nil {
		s.affinity.SetSpill(*u.AffinitySpill)
	}
	if timeout > 0 {
		s.gw.SetUpstreamTimeout(timeout)
	}
	if u.ThermalLimitC != nil {
		s.SetThermalLimitC(*u.ThermalLimitC)
	}
	return 0, nil
}

func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}
