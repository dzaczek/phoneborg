package controller

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/proto"
)

// Admin API for model management (ADR-011).

// ModelList is the response of GET /admin/models.
type ModelList struct {
	Models []models.Model `json:"models"`
}

func (s *Server) registerModelAdmin(add func(pattern, action string, fn http.HandlerFunc)) {
	add("GET /admin/models", "models_list", s.adminModels)
	add("POST /admin/models", "model_add", s.adminAddModel)
	add("PATCH /admin/models/{id}", "model_update", s.adminUpdateModel)
	add("DELETE /admin/models/{id}", "model_delete", s.adminDeleteModel)
	add("GET /admin/device-classes", "classes_list", s.adminDeviceClasses)
	add("GET /admin/placement", "placement_get", s.adminPlacement)
	add("PUT /admin/placement", "placement_set", s.adminSetPlacement)
	add("POST /admin/placement/preview", "placement_preview", s.adminPreviewPlacement)
}

// withState fills in the fields the catalog does not own.
func (s *Server) withState(ms []models.Model) []models.Model {
	nodes, _ := s.reg.View()
	serving := nodesServing(nodes)
	s.place.mu.Lock()
	def := s.place.spec.DefaultModel
	s.place.mu.Unlock()
	for i := range ms {
		ms[i].Default = ms[i].ID == def
		ms[i].NodesServing = serving[ms[i].ID]
	}
	return ms
}

func (s *Server) adminModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ModelList{Models: s.withState(s.catalog.List())})
}

func modelError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, models.ErrInvalid):
		httpError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, models.ErrExists):
		httpError(w, http.StatusConflict, err.Error())
	case errors.Is(err, models.ErrUnknownModel):
		httpError(w, http.StatusNotFound, err.Error())
	default:
		httpError(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) adminAddModel(w http.ResponseWriter, r *http.Request) {
	var req models.AddRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	m, err := s.catalog.Add(req)
	s.audit(r, "model_add", err, "model_id", m.ID, "source", req.Source)
	if err != nil && m.ID == "" {
		modelError(w, err)
		return
	}
	// A failed catalog save still leaves the download running; it is
	// logged by the catalog and retried on the next change.
	writeJSON(w, http.StatusAccepted, s.withState([]models.Model{m})[0])
}

func (s *Server) adminUpdateModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var p models.Patch
	if !decodeStrict(w, r, &p) {
		return
	}
	if _, ok := s.catalog.Get(id); !ok {
		s.audit(r, "model_update", models.ErrUnknownModel, "model_id", id)
		modelError(w, models.ErrUnknownModel)
		return
	}
	m, err := s.catalog.Update(id, p)
	if err == nil && p.Default != nil {
		err = s.setDefault(id, *p.Default)
	}
	attrs := []any{"model_id", id}
	if p.Default != nil {
		attrs = append(attrs, "default", *p.Default)
	}
	s.audit(r, "model_update", err, attrs...)
	if err != nil {
		modelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.withState([]models.Model{m})[0])
}

// setDefault makes id the default model, or clears the default if it is id.
func (s *Server) setDefault(id string, on bool) error {
	p := s.place
	p.mu.Lock()
	switch {
	case on:
		p.spec.DefaultModel = id
	case p.spec.DefaultModel == id:
		p.spec.DefaultModel = ""
	default:
		p.mu.Unlock()
		return nil
	}
	err := p.save()
	p.mu.Unlock()
	s.Replan()
	return err
}

func (s *Server) adminDeleteModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.place.mu.Lock()
	var users []string
	for _, pol := range s.place.spec.Policies {
		if pol.ModelID == id {
			users = append(users, pol.Mode+" policy")
		}
	}
	if s.place.spec.DefaultModel == id {
		users = append(users, "default_model")
	}
	s.place.mu.Unlock()
	if len(users) > 0 {
		err := fmt.Errorf("model %s is used by the placement (%s); change the placement first", id, strings.Join(users, ", "))
		s.audit(r, "model_delete", err, "model_id", id)
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	err := s.catalog.Remove(id)
	s.audit(r, "model_delete", err, "model_id", id)
	if err != nil {
		modelError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminDeviceClasses(w http.ResponseWriter, r *http.Request) {
	nodes, _ := s.reg.View()
	count, tierCount := map[string]int{}, map[string]int{}
	for _, n := range nodes {
		if n.State != proto.StateOffline {
			count[models.ClassOf(n.Inventory.RAMTotalBytes)]++
			tierCount[models.PerfTierOf(s.perf.get(n.ID).GenGBps)]++
		}
	}
	ms := s.catalog.List()
	out := DeviceClasses{Classes: make([]DeviceClass, 0, len(models.Classes)), PerfTiers: make([]PerfTierCount, 0, len(models.PerfTiers))}
	for _, c := range models.Classes {
		dc := DeviceClass{Class: c, Nodes: count[c.ID], RecommendedModels: []string{}}
		for _, m := range ms {
			if slices.Contains(m.RecommendedClasses, c.ID) {
				dc.RecommendedModels = append(dc.RecommendedModels, m.ID)
			}
		}
		out.Classes = append(out.Classes, dc)
	}
	for _, t := range models.PerfTiers {
		tc := PerfTierCount{PerfTier: t, Nodes: tierCount[t.ID], RecommendedModels: []string{}}
		for _, m := range ms {
			if slices.Contains(m.RecommendedTiers, t.ID) {
				tc.RecommendedModels = append(tc.RecommendedModels, m.ID)
			}
		}
		out.PerfTiers = append(out.PerfTiers, tc)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminPlacement(w http.ResponseWriter, r *http.Request) {
	s.place.mu.Lock()
	out := s.place.view()
	s.place.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// validatePlacement checks spec against all known nodes (offline ones too,
// so a pin survives a phone being unplugged) and catalog models.
func (s *Server) validatePlacement(spec models.Spec) error {
	nodes, _ := s.reg.View()
	var nodeIDs, modelIDs []string
	for _, n := range nodes {
		nodeIDs = append(nodeIDs, n.ID)
	}
	for _, m := range s.catalog.List() {
		modelIDs = append(modelIDs, m.ID)
	}
	return models.Validate(spec, nodeIDs, modelIDs)
}

func placementAttrs(spec models.Spec) []any {
	var ps []string
	for _, p := range spec.Policies {
		ps = append(ps, p.ModelID+":"+p.Mode)
	}
	return []any{"policies", strings.Join(ps, ","), "default_model", spec.DefaultModel}
}

func (s *Server) adminSetPlacement(w http.ResponseWriter, r *http.Request) {
	var spec models.Spec
	if !decodeStrict(w, r, &spec) {
		return
	}
	if err := s.validatePlacement(spec); err != nil {
		s.audit(r, "placement_set", err, placementAttrs(spec)...)
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	p := s.place
	p.mu.Lock()
	old := p.spec
	p.spec = spec.Normalized()
	err := p.save()
	if err != nil {
		p.spec = old
	}
	p.mu.Unlock()
	s.audit(r, "placement_set", err, placementAttrs(spec)...)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "cannot store placement: "+err.Error())
		return
	}
	s.Replan()
	s.adminPlacement(w, r)
}

func (s *Server) adminPreviewPlacement(w http.ResponseWriter, r *http.Request) {
	var spec models.Spec
	if !decodeStrict(w, r, &spec) {
		return
	}
	if err := s.validatePlacement(spec); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	p := s.place
	p.mu.Lock()
	nodes, ready, view := s.planInputs(p.byNode)
	p.mu.Unlock()
	plan := p.planner.Plan(spec, nodes, ready)
	preview := placement{spec: spec, plan: plan, nodes: view}
	writeJSON(w, http.StatusOK, preview.view())
}

// modelCollector exports catalog and placement gauges at scrape time.
type modelCollector struct{ s *Server }

var (
	descModelInfo     = prometheus.NewDesc("phoneborg_model_info", "Catalog models (1 per model) with GGUF metadata.", []string{"model_id", "arch", "params", "quant"}, nil)
	descModelProgress = prometheus.NewDesc("phoneborg_model_download_progress", "Controller-side download progress of a catalog model, 0..1 (1 = ready).", []string{"model_id"}, nil)
	descNodeModel     = prometheus.NewDesc("phoneborg_node_model", "1 per node: the model it serves or is switching to, and its state (serving, downloading, loading, error, idle).", []string{"node_id", "model_id", "state"}, nil)
	descPlanNodes     = prometheus.NewDesc("phoneborg_placement_plan_nodes", "Nodes the current placement plan gives each model.", []string{"model_id"}, nil)
)

func (c *modelCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descModelInfo, descModelProgress, descNodeModel, descPlanNodes} {
		ch <- d
	}
}

func (c *modelCollector) Collect(ch chan<- prometheus.Metric) {
	g := prometheus.GaugeValue
	planned := map[string]int{}
	for _, m := range c.s.catalog.List() {
		ch <- prometheus.MustNewConstMetric(descModelInfo, g, 1, m.ID, m.Arch, m.Params, m.Quant)
		ch <- prometheus.MustNewConstMetric(descModelProgress, g, m.Progress, m.ID)
		planned[m.ID] = 0
	}
	c.s.place.mu.Lock()
	for _, a := range c.s.place.plan.Assignments {
		if a.ModelID != "" {
			planned[a.ModelID]++
		}
	}
	c.s.place.mu.Unlock()
	for id, n := range planned {
		ch <- prometheus.MustNewConstMetric(descPlanNodes, g, float64(n), id)
	}
	nodes, _ := c.s.reg.View()
	for _, n := range nodes {
		if n.State == proto.StateOffline || n.LastHeartbeat == nil || n.LastHeartbeat.Runtime == nil {
			continue
		}
		rt := n.LastHeartbeat.Runtime
		ch <- prometheus.MustNewConstMetric(descNodeModel, g, 1, n.ID, currentModel(rt), runtimeState(rt))
	}
}
