package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/proto"
)

// Model management (ADR-011): the controller keeps a model catalog, plans
// which model each node serves, and tells nodes in heartbeat replies.

// PathModelFiles serves catalog model files to agents: GET <prefix><model_id>.
const PathModelFiles = "/v1/model-files/"

// ModelOptions configures model management.
type ModelOptions struct {
	// Catalog holds the models; nil = an in-memory catalog whose files go
	// to a temporary directory.
	Catalog *models.Catalog
	// PlacementFile persists policies and the default model; "" = memory.
	PlacementFile string
	// Placement is the initial placement (see LoadPlacement).
	Placement models.Spec
	// Planner computes plans; nil = models.DefaultPlanner.
	Planner models.Planner
}

// Placement is the response of GET/PUT /admin/placement and
// POST /admin/placement/preview.
type Placement struct {
	Policies     []models.Policy     `json:"policies"`
	DefaultModel string              `json:"default_model"`
	Plan         []models.Assignment `json:"plan"`
	Nodes        []PlacementNode     `json:"nodes"`
	Warnings     []string            `json:"warnings"`
}

// PlacementNode is a planned node's current state.
type PlacementNode struct {
	NodeID        string  `json:"node_id"`
	Class         string  `json:"class"`
	RAMTotalBytes uint64  `json:"ram_total_bytes"`
	CurrentModel  string  `json:"current_model"`
	State         string  `json:"state"` // serving | downloading | loading | error | idle
	Progress      float64 `json:"progress"`
	Error         string  `json:"error"`
	Drained       bool    `json:"drained"`
	// BudgetBytes is the node's self-reported memory budget (ADR-012); 0 =
	// unknown, the planner falls back to the class heuristic for fit.
	BudgetBytes int64 `json:"budget_bytes,omitempty"`
}

// DeviceClass is one row of GET /admin/device-classes.
type DeviceClass struct {
	models.Class
	Nodes             int      `json:"nodes"`
	RecommendedModels []string `json:"recommended_models"`
}

// DeviceClasses is the response of GET /admin/device-classes.
type DeviceClasses struct {
	Classes []DeviceClass `json:"classes"`
}

type placementFile struct {
	Version int `json:"version"`
	models.Spec
}

// LoadPlacement reads a placement file; a missing file is an empty placement.
func LoadPlacement(path string) (models.Spec, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return models.Spec{}, nil
	}
	if err != nil {
		return models.Spec{}, err
	}
	var f placementFile
	if err := json.Unmarshal(data, &f); err != nil {
		return models.Spec{}, fmt.Errorf("%s: %w (move it away to start without policies)", path, err)
	}
	if f.Version != 1 {
		return models.Spec{}, fmt.Errorf("%s: unsupported version %d", path, f.Version)
	}
	return f.Spec, nil
}

// placement holds the current spec and the last plan.
type placement struct {
	planner models.Planner
	path    string

	mu     sync.Mutex
	spec   models.Spec
	plan   models.Plan
	nodes  []PlacementNode
	byNode map[string]models.Assignment
}

func (p *placement) save() error {
	if p.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(placementFile{Version: 1, Spec: p.spec.Normalized()}, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	return gateway.WriteFileAtomic(p.path, data, 0o600)
}

// assigning reports whether a plan reason makes the controller send a
// desired runtime (as opposed to letting the node keep what it has).
func assigning(reason string) bool {
	switch reason {
	case models.ReasonPin, models.ReasonReplicas, models.ReasonPercent, models.ReasonDefault:
		return true
	}
	return false
}

// runtimeState is a node's model state, derived from its last heartbeat
// for agents that do not report State.
func runtimeState(rt *proto.RuntimeStatus) string {
	switch {
	case rt == nil:
		return "idle"
	case rt.State != "":
		return rt.State
	case rt.Ready:
		return "serving"
	}
	return "loading"
}

// currentModel is the model a node serves or is switching to.
func currentModel(rt *proto.RuntimeStatus) string {
	switch {
	case rt == nil:
		return ""
	case rt.ModelID != "":
		return rt.ModelID
	}
	return rt.Model
}

// planInputs snapshots nodes and ready models for the planner. OFFLINE
// nodes are not planned. prev is the previous plan by node, for stability.
func (s *Server) planInputs(prev map[string]models.Assignment) ([]models.Node, []models.PlanModel, []PlacementNode) {
	all, drained := s.reg.View()
	var nodes []models.Node
	view := []PlacementNode{}
	for _, n := range all {
		if n.State == proto.StateOffline {
			continue
		}
		var rt *proto.RuntimeStatus
		if n.LastHeartbeat != nil {
			rt = n.LastHeartbeat.Runtime
		}
		pn := models.Node{ID: n.ID, Class: models.ClassOf(n.Inventory.RAMTotalBytes), RAMTotalBytes: n.Inventory.RAMTotalBytes,
			CurrentModel: currentModel(rt), Drained: drained[n.ID]}
		if rt != nil {
			pn.Speed = rt.GenTPS
			pn.BudgetBytes = rt.BudgetBytes
		}
		if a := prev[n.ID]; assigning(a.Reason) {
			pn.Assigned = a.ModelID
		}
		nodes = append(nodes, pn)
		v := PlacementNode{NodeID: n.ID, Class: pn.Class, RAMTotalBytes: pn.RAMTotalBytes, CurrentModel: pn.CurrentModel,
			State: runtimeState(rt), Drained: pn.Drained, BudgetBytes: pn.BudgetBytes}
		if rt != nil {
			v.Progress, v.Error = rt.Progress, rt.Error
		}
		view = append(view, v)
	}
	var ready []models.PlanModel
	for _, m := range s.catalog.List() {
		if m.Status == models.StatusReady {
			ready = append(ready, m.PlanModel())
		}
	}
	return nodes, ready, view
}

// Replan recomputes the plan from the current spec, nodes and catalog.
// It runs when placement, nodes or models change, and periodically.
func (s *Server) Replan() {
	p := s.place
	p.mu.Lock()
	defer p.mu.Unlock()
	nodes, ready, view := s.planInputs(p.byNode)
	plan := p.planner.Plan(p.spec, nodes, ready)
	byNode := make(map[string]models.Assignment, len(plan.Assignments))
	for _, a := range plan.Assignments {
		byNode[a.NodeID] = a
		if old := p.byNode[a.NodeID]; assigning(a.Reason) && (old.ModelID != a.ModelID || !assigning(old.Reason)) {
			s.log.Info("placement changed", "node_id", a.NodeID, "model_id", a.ModelID, "from_model_id", old.ModelID,
				"reason", a.Reason, "est_ram_bytes", a.EstRAMBytes, "fits", a.Fits)
		}
	}
	if !slices.Equal(plan.Warnings, p.plan.Warnings) {
		for _, w := range plan.Warnings {
			s.log.Warn("placement warning", "warning", w)
		}
	}
	p.plan, p.nodes, p.byNode = plan, view, byNode
}

// placementView returns the current placement. Call with p.mu held.
func (p *placement) view() Placement {
	spec := p.spec.Normalized()
	plan := p.plan.Assignments
	if plan == nil {
		plan = []models.Assignment{}
	}
	nodes, warnings := p.nodes, p.plan.Warnings
	if nodes == nil {
		nodes = []PlacementNode{}
	}
	if warnings == nil {
		warnings = []string{}
	}
	return Placement{Policies: spec.Policies, DefaultModel: spec.DefaultModel, Plan: plan, Nodes: nodes, Warnings: warnings}
}

// desired returns what a node should serve, or nil to let it keep its
// current runtime (no policy for it, or the model is not ready).
func (s *Server) desired(nodeID string) *proto.DesiredRuntime {
	s.place.mu.Lock()
	a := s.place.byNode[nodeID]
	s.place.mu.Unlock()
	if !assigning(a.Reason) {
		return nil
	}
	_, m, ok := s.catalog.File(a.ModelID)
	if !ok {
		return nil
	}
	return &proto.DesiredRuntime{ModelID: m.ID, URL: PathModelFiles + m.ID, SHA256: m.SHA256, SizeBytes: m.SizeBytes,
		CtxSize: a.CtxSize, Slots: a.Slots, KVType: a.KVType, Layers: m.Layers, KVHeads: m.KVHeads, HeadDim: m.HeadDim}
}

// nodesServing counts ACTIVE nodes whose runtime is ready, by model.
func nodesServing(nodes []proto.Node) map[string]int {
	out := map[string]int{}
	for _, n := range nodes {
		if n.State != proto.StateActive || n.LastHeartbeat == nil || n.LastHeartbeat.Runtime == nil || !n.LastHeartbeat.Runtime.Ready {
			continue
		}
		out[currentModel(n.LastHeartbeat.Runtime)]++
	}
	return out
}
