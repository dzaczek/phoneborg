package controller

import (
	"net/http"
	"strings"
	"testing"
)

func TestPoolStatusDisableAndConflicts(t *testing.T) {
	e := newRoutingEnv(t, "", "a", "b") // both serve model "m"
	e.admin(http.MethodPatch, "/admin/nodes/a", `{"alias":"oneplus"}`, 200, nil)

	var p Pool
	e.admin(http.MethodPut, "/admin/pools/main", `{"models":["m"],"nodes":["oneplus"]}`, 200, &p)
	if p.Status != PoolReady || p.StatusReason != "1 node(s)" {
		t.Fatalf("status %q %q", p.Status, p.StatusReason)
	}
	e.admin(http.MethodPut, "/admin/pools/big", `{"models":["other"]}`, 200, &p) // any node: claims none
	if p.Status != PoolNoReadyNode || !strings.Contains(p.StatusReason, "no node serves an allowed model (other)") {
		t.Fatalf("status %q %q", p.Status, p.StatusReason)
	}

	// A second enabled pool naming oneplus with no common model conflicts.
	w := e.do(http.MethodPut, "/admin/pools/rival", `{"models":["other"],"nodes":["a"]}`, testToken)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "pool/rival and pool/main both claim node a but allow no common model") {
		t.Fatalf("conflict: %d %s", w.Code, w.Body)
	}
	e.admin(http.MethodPut, "/admin/pools/rival", `{"models":["other","m"],"nodes":["a"]}`, 200, nil)            // a shared model is fine
	e.admin(http.MethodPut, "/admin/pools/rival", `{"models":["other"],"nodes":["a"],"disabled":true}`, 200, &p) // so is a disabled pool
	if p.Status != PoolDisabled {
		t.Fatalf("status %q", p.Status)
	}
	// Enabling it again is checked too.
	e.admin(http.MethodPut, "/admin/pools/rival", `{"models":["other"],"nodes":["a"]}`, 409, nil)

	// A disabled pool is not listed and refuses requests.
	if m := e.do(http.MethodGet, "/v1/models", "", "").Body.String(); strings.Contains(m, "pool/rival") || !strings.Contains(m, "pool/main") {
		t.Errorf("/v1/models: %s", m)
	}
	w = e.do(http.MethodPost, "/v1/chat/completions", `{"model":"pool/rival","messages":[{"role":"user","content":"x"}]}`, "")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "pool_disabled") {
		t.Errorf("disabled pool request: %d %s", w.Code, w.Body)
	}
	e.admin(http.MethodPost, "/admin/prewarm", `{"target":"pool/rival","messages":[{"role":"system","content":"x"}]}`, 200, nil)
	if m := e.do(http.MethodGet, "/metrics", "", "").Body.String(); !strings.Contains(m, `phoneborg_gateway_rejected_total{reason="pool_disabled"} 1`) {
		t.Error("rejection not counted")
	}
}
