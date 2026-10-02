package controller

import (
	"errors"
	"net/http"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

// Super Borg mode (ADR-020): the cluster answers as one model, an
// orchestrator node delegates subtasks to the other nodes. The gateway runs
// it (controller/gateway/superborg.go); the controller stores the settings
// in routing.json and serves them on the admin API.

// SuperborgSettings is the body of PUT /admin/superborg and what
// routing.json persists.
type SuperborgSettings struct {
	Enabled bool `json:"enabled"`
	// Orchestrator is a node alias or id; "" = the ready node serving the
	// largest model, which is also the fallback while it is not ready.
	Orchestrator string `json:"orchestrator"`
	// Thinking lets the orchestrator reason before it plans or answers.
	Thinking bool `json:"thinking"`
}

// SuperborgStatus is the response of GET/PUT /admin/superborg: the settings
// plus the nodes a request would use right now.
type SuperborgStatus struct {
	SuperborgSettings
	// ActiveOrchestrator is the node id that orchestrates now, "" when no
	// node is ready.
	ActiveOrchestrator string            `json:"active_orchestrator"`
	OrchestratorModel  string            `json:"orchestrator_model"`
	Workers            []SuperborgWorker `json:"workers"`
}

// SuperborgWorker is one worker node in SuperborgStatus.
type SuperborgWorker struct {
	NodeID string  `json:"node_id"`
	Alias  string  `json:"alias"`
	Model  string  `json:"model"`
	GenTPS float64 `json:"gen_tps"`
}

// applySuperborg stores sb and switches the gateway's mode.
func (s *Server) applySuperborg(sb SuperborgSettings) {
	s.superborg.Store(&sb)
	if !sb.Enabled {
		s.gw.SetSuperborg(nil)
		return
	}
	s.gw.SetSuperborg(&gateway.Superborg{Orchestrator: sb.Orchestrator, Thinking: sb.Thinking})
}

func (s *Server) superborgStatus() SuperborgStatus {
	st := SuperborgStatus{SuperborgSettings: *s.superborg.Load(), Workers: []SuperborgWorker{}}
	orch, workers, ok := s.gw.SuperborgPlan(gateway.Superborg{Orchestrator: st.Orchestrator})
	if !ok {
		return st
	}
	st.ActiveOrchestrator, st.OrchestratorModel = orch.NodeID, orch.Model
	for _, w := range workers {
		st.Workers = append(st.Workers, SuperborgWorker{NodeID: w.NodeID, Alias: w.Alias, Model: w.Model, GenTPS: w.Speed})
	}
	return st
}

func (s *Server) registerSuperborgAdmin(add func(pattern, action string, fn http.HandlerFunc)) {
	add("GET /admin/superborg", "superborg_get", s.adminSuperborg)
	add("PUT /admin/superborg", "superborg_set", s.adminSetSuperborg)
}

func (s *Server) adminSuperborg(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.superborgStatus())
}

func (s *Server) adminSetSuperborg(w http.ResponseWriter, r *http.Request) {
	var sb SuperborgSettings
	if !decodeStrict(w, r, &sb) {
		return
	}
	if sb.Orchestrator != "" {
		if _, known := s.reg.Lookup(sb.Orchestrator); !known && !s.ext.has(sb.Orchestrator) {
			err := errors.New("unknown node " + sb.Orchestrator + ` (use an alias or node id, or "" for automatic)`)
			s.audit(r, "superborg_set", err, "orchestrator", sb.Orchestrator)
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	prev := *s.superborg.Load()
	s.applySuperborg(sb)
	err := s.saveRouting()
	if err != nil {
		s.applySuperborg(prev)
	}
	s.audit(r, "superborg_set", err, "enabled", sb.Enabled, "orchestrator", sb.Orchestrator, "thinking", sb.Thinking)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "cannot store routing state: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.superborgStatus())
}
