package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/proto"
)

// Virtual models, pools and node aliases (ADR-014).

// Pool routing values.
const (
	RoutingSpread   = "spread"   // least in-flight, then fastest; no session pinning
	RoutingAffinity = "affinity" // the gateway's normal policy
)

// Eligibility reasons of pool members.
const (
	ReasonNotMember    = "not a member"
	ReasonClass        = "class not allowed"
	ReasonDrained      = "drained"
	ReasonHot          = "hot"
	ReasonSwitching    = "switching"
	ReasonNotReady     = "not ready"
	ReasonModel        = "model not allowed"
	ReasonBelowMinTPS  = "below min_gen_tps"
	poolPrefix         = gateway.KindPool + "/"
	nodePrefix         = gateway.KindNode + "/"
	routingFileVersion = 1
)

// Pool is a named set of nodes that clients address as "pool/<name>".
type Pool struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Models      []string `json:"models"`      // allowed served models; empty = any
	Nodes       []string `json:"nodes"`       // aliases or node ids; empty = any
	Classes     []string `json:"classes"`     // device classes; empty = any
	MinGenTPS   float64  `json:"min_gen_tps"` // self-test generation tok/s; 0 = no limit
	Routing     string   `json:"routing"`     // RoutingSpread (default) or RoutingAffinity
	// Members is computed in responses and ignored in requests.
	Members []PoolMember `json:"members,omitempty"`
}

// PoolMember is one node's eligibility for a pool.
type PoolMember struct {
	NodeID   string `json:"node_id"`
	Alias    string `json:"alias"`
	Model    string `json:"model"`
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
}

// Pools is the response of GET /admin/pools.
type Pools struct {
	Pools []Pool `json:"pools"`
}

// AliasUpdate is the body of PATCH /admin/nodes/{id}.
type AliasUpdate struct {
	Alias *string `json:"alias"`
}

// PrewarmRequest is the body of POST /admin/prewarm.
type PrewarmRequest struct {
	Target   string            `json:"target"`
	Messages []json.RawMessage `json:"messages"`
	Tools    json.RawMessage   `json:"tools,omitempty"`
}

// PrewarmResponse is the response of POST /admin/prewarm.
type PrewarmResponse struct {
	Results []gateway.PrewarmResult `json:"results"`
}

// RoutingState is what the routing file persists: node aliases by node id
// and pools.
type RoutingState struct {
	Aliases map[string]string `json:"aliases"`
	Pools   []Pool            `json:"pools"`
}

// RoutingOptions configures aliases and pools.
type RoutingOptions struct {
	File  string       // persists aliases and pools; "" = memory only
	State RoutingState // initial state (see LoadRouting)
}

type routingFile struct {
	Version int `json:"version"`
	RoutingState
}

// LoadRouting reads a routing file; a missing file is an empty state.
func LoadRouting(path string) (RoutingState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return RoutingState{}, nil
	}
	if err != nil {
		return RoutingState{}, err
	}
	var f routingFile
	if err := json.Unmarshal(data, &f); err != nil {
		return RoutingState{}, fmt.Errorf("%s: %w (move it away to start without aliases and pools)", path, err)
	}
	if f.Version != routingFileVersion {
		return RoutingState{}, fmt.Errorf("%s: unsupported version %d", path, f.Version)
	}
	for i, p := range f.Pools {
		if f.Pools[i], err = normPool(p); err != nil {
			return RoutingState{}, fmt.Errorf("%s: pool %q: %w", path, p.Name, err)
		}
	}
	return f.RoutingState, nil
}

// pools holds the pool definitions.
type pools struct {
	path string

	mu     sync.Mutex // also serialises saves
	byName map[string]Pool
}

// normPool validates a pool definition and fills defaults. Members are dropped.
func normPool(p Pool) (Pool, error) {
	p.Members = nil
	if !nameRE.MatchString(p.Name) {
		return p, errors.New("pool name must match ^[a-z0-9][a-z0-9-]{0,31}$")
	}
	clean := func(field string, vs []string) ([]string, error) {
		out := []string{}
		for _, v := range vs {
			v = strings.TrimSpace(v)
			if v == "" {
				return nil, fmt.Errorf("%s: empty entry", field)
			}
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
		return out, nil
	}
	var err error
	if p.Models, err = clean("models", p.Models); err != nil {
		return p, err
	}
	if p.Nodes, err = clean("nodes", p.Nodes); err != nil {
		return p, err
	}
	if p.Classes, err = clean("classes", p.Classes); err != nil {
		return p, err
	}
	for _, c := range p.Classes {
		if !models.IsClass(c) {
			return p, fmt.Errorf("unknown device class %q", c)
		}
	}
	if p.MinGenTPS < 0 {
		return p, errors.New("min_gen_tps must not be negative")
	}
	switch p.Routing {
	case "":
		p.Routing = RoutingSpread
	case RoutingSpread, RoutingAffinity:
	default:
		return p, fmt.Errorf("routing must be %q or %q", RoutingSpread, RoutingAffinity)
	}
	return p, nil
}

// list returns the pools sorted by name, without members.
func (ps *pools) list() []Pool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make([]Pool, 0, len(ps.byName))
	for _, p := range ps.byName {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (ps *pools) get(name string) (Pool, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p, ok := ps.byName[name]
	return p, ok
}

// saveRouting writes aliases and pools to the routing file, if any.
func (s *Server) saveRouting() error {
	ps := s.pools
	if ps.path == "" {
		return nil
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	st := RoutingState{Aliases: s.reg.Aliases(), Pools: []Pool{}}
	for _, p := range ps.byName {
		st.Pools = append(st.Pools, p)
	}
	sort.Slice(st.Pools, func(i, j int) bool { return st.Pools[i].Name < st.Pools[j].Name })
	data, err := json.MarshalIndent(routingFile{Version: routingFileVersion, RoutingState: st}, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ps.path), 0o700); err != nil {
		return err
	}
	return gateway.WriteFileAtomic(ps.path, data, 0o600)
}

// nodeReason is why a node cannot take new pool requests right now, or ""
// if it can. It mirrors Backends plus drain and the thermal limit.
func nodeReason(n proto.Node, drained bool, thermalLimitC float64) string {
	hb := n.LastHeartbeat
	switch {
	case drained:
		return ReasonDrained
	case hb != nil && hb.Runtime != nil && switching(hb.Runtime):
		return ReasonSwitching
	case n.State != proto.StateActive || hb == nil || hb.Runtime == nil || !hb.Runtime.Ready || hb.Runtime.AdvertisePort == 0:
		return ReasonNotReady
	case thermalLimitC > 0 && hb.TemperatureC != nil && *hb.TemperatureC >= thermalLimitC:
		return ReasonHot
	}
	return ""
}

// poolMembers computes every node's eligibility for p.
func poolMembers(p Pool, nodes []proto.Node, drained map[string]bool, thermalLimitC float64) []PoolMember {
	out := []PoolMember{}
	for _, n := range nodes {
		m := PoolMember{NodeID: n.ID, Alias: n.Alias}
		var rt *proto.RuntimeStatus
		if n.LastHeartbeat != nil {
			rt = n.LastHeartbeat.Runtime
		}
		if rt != nil {
			m.Model = rt.Model
		}
		switch {
		case len(p.Nodes) > 0 && !slices.Contains(p.Nodes, n.ID) && (n.Alias == "" || !slices.Contains(p.Nodes, n.Alias)):
			m.Reason = ReasonNotMember
		case len(p.Classes) > 0 && !slices.Contains(p.Classes, models.ClassOf(n.Inventory.RAMTotalBytes)):
			m.Reason = ReasonClass
		default:
			m.Reason = nodeReason(n, drained[n.ID], thermalLimitC)
		}
		if m.Reason == "" && len(p.Models) > 0 && !slices.Contains(p.Models, rt.Model) {
			m.Reason = ReasonModel
		}
		if m.Reason == "" && p.MinGenTPS > 0 && rt.GenTPS < p.MinGenTPS {
			m.Reason = ReasonBelowMinTPS
		}
		m.Eligible = m.Reason == ""
		out = append(out, m)
	}
	return out
}

// withMembers returns p with its members computed from the registry.
func (s *Server) withMembers(p Pool) Pool {
	nodes, drained := s.reg.View()
	p.Members = poolMembers(p, nodes, drained, s.ThermalLimitC())
	return p
}

func eligible(members []PoolMember) int {
	n := 0
	for _, m := range members {
		if m.Eligible {
			n++
		}
	}
	return n
}

// serverTargets resolves pools and node aliases for the gateway.
type serverTargets struct{ s *Server }

func (t serverTargets) Resolve(name string) (gateway.Target, bool) {
	s := t.s
	if pn, ok := strings.CutPrefix(name, poolPrefix); ok {
		p, ok := s.pools.get(pn)
		if !ok {
			return gateway.Target{}, false
		}
		tgt := gateway.Target{Label: name, Nodes: map[string]bool{}}
		for _, m := range s.withMembers(p).Members {
			if m.Eligible {
				tgt.Nodes[m.NodeID] = true
			}
		}
		if p.Routing == RoutingSpread {
			tgt.Picker = s.spread
		}
		return tgt, true
	}
	if nn, ok := strings.CutPrefix(name, nodePrefix); ok {
		if id, ok := s.reg.Lookup(nn); ok {
			return gateway.Target{Label: name, Node: id}, true
		}
	}
	return gateway.Target{}, false
}

func (t serverTargets) Models() []gateway.ModelEntry {
	s := t.s
	nodes, drained := s.reg.View()
	limit := s.ThermalLimitC()
	var out []gateway.ModelEntry
	for _, p := range s.pools.list() {
		out = append(out, gateway.ModelEntry{ID: poolPrefix + p.Name, Object: "model", OwnedBy: "phoneborg", Kind: gateway.KindPool,
			Nodes: eligible(poolMembers(p, nodes, drained, limit)), Description: p.Description})
	}
	for _, n := range nodes {
		if n.Alias == "" {
			continue
		}
		e := gateway.ModelEntry{ID: nodePrefix + n.Alias, Object: "model", OwnedBy: "phoneborg", Kind: gateway.KindNode, Nodes: 1}
		if n.LastHeartbeat != nil && n.LastHeartbeat.Runtime != nil {
			e.Model = n.LastHeartbeat.Runtime.Model
		}
		reason := nodeReason(n, drained[n.ID], limit)
		ready := reason == "" || reason == ReasonHot // a hot node still serves explicit requests
		e.Ready = &ready
		out = append(out, e)
	}
	return out
}

// poolCollector exports each pool's eligible member count at scrape time.
type poolCollector struct{ s *Server }

var descPoolMembers = prometheus.NewDesc("phoneborg_pool_members", "Eligible members of each pool (ADR-014).", []string{"pool"}, nil)

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) { ch <- descPoolMembers }

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	nodes, drained := c.s.reg.View()
	limit := c.s.ThermalLimitC()
	for _, p := range c.s.pools.list() {
		ch <- prometheus.MustNewConstMetric(descPoolMembers, prometheus.GaugeValue,
			float64(eligible(poolMembers(p, nodes, drained, limit))), p.Name)
	}
}

// Admin handlers.

func (s *Server) registerPoolAdmin(add func(pattern, action string, fn http.HandlerFunc)) {
	add("PATCH /admin/nodes/{id}", "node_alias", s.adminSetAlias)
	add("GET /admin/pools", "pools_list", s.adminPools)
	add("PUT /admin/pools/{name}", "pool_set", s.adminSetPool)
	add("DELETE /admin/pools/{name}", "pool_delete", s.adminDeletePool)
	add("POST /admin/prewarm", "prewarm", s.adminPrewarm)
}

func (s *Server) adminSetAlias(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var u AliasUpdate
	if !decodeStrict(w, r, &u) {
		return
	}
	if u.Alias == nil {
		httpError(w, http.StatusBadRequest, `body must be {"alias": "<alias>"} ("" clears it)`)
		return
	}
	err := s.reg.SetAlias(id, *u.Alias)
	if err == nil {
		err = s.saveRouting()
	}
	s.audit(r, "node_alias", err, "node_id", id, "alias", *u.Alias)
	switch {
	case errors.Is(err, ErrAliasInvalid):
		httpError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, ErrAliasTaken):
		httpError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		notFoundOr500(w, err)
		return
	}
	for _, n := range s.adminNodeList() {
		if n.ID == id {
			writeJSON(w, http.StatusOK, n)
			return
		}
	}
	writeJSON(w, http.StatusOK, AdminNode{Node: proto.Node{ID: id}}) // forgotten node, alias cleared
}

func (s *Server) adminPools(w http.ResponseWriter, r *http.Request) {
	out := Pools{Pools: []Pool{}}
	for _, p := range s.pools.list() {
		out.Pools = append(out.Pools, s.withMembers(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminSetPool(w http.ResponseWriter, r *http.Request) {
	var p Pool
	if !decodeStrict(w, r, &p) {
		return
	}
	p.Name = r.PathValue("name")
	p, err := normPool(p)
	if err != nil {
		s.audit(r, "pool_set", err, "pool", p.Name)
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.pools.mu.Lock()
	s.pools.byName[p.Name] = p
	s.pools.mu.Unlock()
	err = s.saveRouting()
	s.audit(r, "pool_set", err, "pool", p.Name, "routing", p.Routing)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "cannot store pools: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.withMembers(p))
}

func (s *Server) adminDeletePool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.pools.mu.Lock()
	_, ok := s.pools.byName[name]
	delete(s.pools.byName, name)
	s.pools.mu.Unlock()
	var err error
	if !ok {
		err = errors.New("unknown pool")
	} else {
		err = s.saveRouting()
	}
	s.audit(r, "pool_delete", err, "pool", name)
	switch {
	case !ok:
		httpError(w, http.StatusNotFound, "unknown pool "+name)
	case err != nil:
		httpError(w, http.StatusInternalServerError, "cannot store pools: "+err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) adminPrewarm(w http.ResponseWriter, r *http.Request) {
	var req PrewarmRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)) // prompts with tool definitions are large
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Target == "" || len(req.Messages) == 0 {
		httpError(w, http.StatusBadRequest, "target and messages are required")
		return
	}
	results, err := s.gw.Prewarm(r.Context(), req.Target, req.Messages, req.Tools)
	ok := 0
	for _, res := range results {
		if res.OK {
			ok++
		}
	}
	s.audit(r, "prewarm", err, "target", req.Target, "nodes", len(results), "ok", ok)
	switch {
	case errors.Is(err, gateway.ErrUnknownTarget):
		httpError(w, http.StatusNotFound, "no pool or node "+req.Target)
	case errors.Is(err, gateway.ErrNodeUnavailable):
		httpError(w, http.StatusServiceUnavailable, req.Target+" is not ready")
	case err != nil:
		httpError(w, http.StatusInternalServerError, err.Error())
	default:
		if results == nil {
			results = []gateway.PrewarmResult{}
		}
		writeJSON(w, http.StatusOK, PrewarmResponse{Results: results})
	}
}
