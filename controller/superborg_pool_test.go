package controller

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

func TestSuperborgPool(t *testing.T) {
	e := newRoutingEnv(t, "", "a", "b", "c")
	e.admin(http.MethodPatch, "/admin/nodes/b", `{"alias":"oneplus"}`, 200, nil)
	var p Pool
	e.admin(http.MethodPut, "/admin/pools/borg", `{"routing":"superborg","nodes":["a","oneplus"],"orchestrator":"oneplus","thinking":true}`, 200, &p)
	if p.Routing != RoutingSuperborg || p.ActiveOrchestrator != "b" || !p.Thinking {
		t.Fatalf("pool %+v", p)
	}
	e.admin(http.MethodPut, "/admin/pools/x", `{"routing":"borg"}`, 400, nil)

	// The pool answers through its orchestrator; other targets are untouched.
	w := e.do(http.MethodPost, "/v1/chat/completions", `{"model":"pool/borg","messages":[{"role":"user","content":"x"}]}`, "")
	if w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "b" || w.Header().Get("X-PhoneBorg-Mode") != "superborg" {
		t.Fatalf("pool/borg: %d %v %s", w.Code, w.Header(), w.Body)
	}
	w = e.do(http.MethodPost, "/v1/chat/completions", `{"model":"node/c","messages":[{"role":"user","content":"x"}]}`, "")
	if w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "c" || w.Header().Get("X-PhoneBorg-Mode") != "" {
		t.Fatalf("node/c: %d %v", w.Code, w.Header())
	}
	if m := e.do(http.MethodGet, "/v1/models", "", "").Body.String(); !strings.Contains(m, `"auto"`) || !strings.Contains(m, `"pool/borg"`) {
		t.Errorf("/v1/models: %s", m)
	}

	// Jobs default to the first Super Borg pool.
	var job gateway.JobSummary
	e.admin(http.MethodPost, "/admin/jobs", `{"goal":"g"}`, 201, &job)
	if job.Pool != "pool/borg" {
		t.Errorf("job pool %q", job.Pool)
	}
	e.admin(http.MethodPost, "/admin/jobs", `{"goal":"g","pool":"nope"}`, 400, nil)
	e.admin(http.MethodPost, "/admin/jobs", `{"goal":"g","pool":"borg"}`, 201, &job)
	if job.Pool != "pool/borg" {
		t.Errorf("job pool %q", job.Pool)
	}
	e.admin(http.MethodGet, "/admin/superborg", "", 404, nil) // the cluster-wide mode is gone
}

func TestSuperborgModeMigratesToPool(t *testing.T) {
	file := filepath.Join(t.TempDir(), "routing.json")
	old := `{"version":1,"aliases":{},"pools":[],"superborg":{"enabled":true,"orchestrator":"oneplus","thinking":true}}`
	if err := os.WriteFile(file, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newRoutingEnv(t, file, "a")
	var pools Pools
	e.admin(http.MethodGet, "/admin/pools", "", 200, &pools)
	if len(pools.Pools) != 1 || pools.Pools[0].Name != "superborg" || pools.Pools[0].Routing != RoutingSuperborg ||
		pools.Pools[0].Orchestrator != "oneplus" || !pools.Pools[0].Thinking {
		t.Fatalf("pools %+v", pools)
	}
	data, _ := os.ReadFile(file)
	if strings.Contains(string(data), `"enabled"`) || !strings.Contains(string(data), `"routing": "superborg"`) {
		t.Errorf("routing.json not rewritten:\n%s", data)
	}
	// A second start does not add it again.
	e2 := newRoutingEnv(t, file, "a")
	e2.admin(http.MethodGet, "/admin/pools", "", 200, &pools)
	if len(pools.Pools) != 1 {
		t.Errorf("pools after restart %+v", pools)
	}
}

func TestExternalOrchestratorRole(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "external.json")
	e := newExtEnv(t, dir, nil)
	api := fakeLlama(t)
	e.admin(http.MethodPut, "/admin/external/deepseek", `{"url":"`+api.URL+`","role":"boss"}`, 400, nil)
	var x External
	e.admin(http.MethodPut, "/admin/external/deepseek", `{"url":"`+api.URL+`","role":"orchestrator"}`, 201, &x)
	if x.Role != ExternalRoleOrchestrator {
		t.Fatalf("role %q", x.Role)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), `"role": "orchestrator"`) && !strings.Contains(string(data), `"role":"orchestrator"`) {
		t.Errorf("role not stored:\n%s", data)
	}
	var p Pool
	e.admin(http.MethodPut, "/admin/pools/any", `{}`, 200, &p)
	for _, m := range p.Members {
		if m.NodeID == "ext:deepseek" && (m.Eligible || m.Reason != ReasonOrchestratorOnly) {
			t.Errorf("member %+v", m)
		}
	}
}
