package controller

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestSuperborgAdmin(t *testing.T) {
	file := filepath.Join(t.TempDir(), "routing.json")
	e := newRoutingEnv(t, file, "a", "b", "c")
	e.admin(http.MethodPatch, "/admin/nodes/b", `{"alias":"oneplus"}`, 200, nil)

	var st SuperborgStatus
	e.admin(http.MethodGet, "/admin/superborg", "", 200, &st)
	if st.Enabled || len(st.Workers) != 2 {
		t.Fatalf("default status %+v", st)
	}
	e.admin(http.MethodPut, "/admin/superborg", `{"enabled":true,"orchestrator":"nope"}`, 400, nil)
	e.admin(http.MethodPut, "/admin/superborg", `{"enabled":true,"bogus":1}`, 400, nil)
	e.admin(http.MethodPut, "/admin/superborg", `{"enabled":true,"orchestrator":"oneplus","thinking":true}`, 200, &st)
	if !st.Enabled || st.ActiveOrchestrator != "b" || len(st.Workers) != 2 || st.Workers[0].NodeID != "a" {
		t.Fatalf("enabled status %+v", st)
	}

	// The cluster is one model; any requested model reaches the orchestrator.
	if w := e.do(http.MethodGet, "/v1/models", "", ""); !strings.Contains(w.Body.String(), `"id":"superborg"`) || strings.Contains(w.Body.String(), `"auto"`) {
		t.Fatalf("/v1/models: %s", w.Body)
	}
	w := e.do(http.MethodPost, "/v1/chat/completions", `{"model":"node/c","messages":[{"role":"user","content":"x"}]}`, "")
	if w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "b" || !strings.Contains(w.Body.String(), `"content":"hi"`) {
		t.Fatalf("chat: %d %s %s", w.Code, w.Header().Get("X-PhoneBorg-Node"), w.Body)
	}
	if m := e.do(http.MethodGet, "/metrics", "", "").Body.String(); !strings.Contains(m, `phoneborg_superborg_requests_total{result="ok"} 1`) {
		t.Errorf("metrics lack the superborg request")
	}

	// The mode survives a restart.
	e2 := newRoutingEnv(t, file, "a", "b")
	e2.admin(http.MethodGet, "/admin/superborg", "", 200, &st)
	if !st.Enabled || st.Orchestrator != "oneplus" || !st.Thinking {
		t.Fatalf("after restart %+v", st)
	}

	e.admin(http.MethodPut, "/admin/superborg", `{"enabled":false}`, 200, &st)
	if w := e.do(http.MethodGet, "/v1/models", "", ""); !strings.Contains(w.Body.String(), `"auto"`) {
		t.Fatalf("/v1/models after disabling: %s", w.Body)
	}
}
