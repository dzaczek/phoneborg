package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/proto"
)

func TestSetAlias(t *testing.T) {
	r, _ := newTestRegistry()
	for _, id := range []string{"a", "b", "phone-02"} {
		r.Register(proto.RegisterRequest{NodeID: id}, "x")
	}
	for _, tc := range []struct {
		id, alias string
		err       error
	}{
		{"a", "phone-01", nil},
		{"a", "phone-01", nil}, // unchanged
		{"b", "phone-01", ErrAliasTaken},
		{"b", "a", ErrAliasTaken},        // another node's id
		{"b", "phone-02", ErrAliasTaken}, // another node's id
		{"phone-02", "phone-02", nil},    // its own id is fine
		{"b", "b", nil},
		{"ghost", "g", ErrUnknownNode},
		{"b", "Phone", ErrAliasInvalid},
		{"b", "-x", ErrAliasInvalid},
		{"b", "x_y", ErrAliasInvalid},
		{"b", strings.Repeat("x", 33), ErrAliasInvalid},
		{"b", strings.Repeat("x", 32), nil},
		{"b", "auto", ErrAliasInvalid},
		{"b", "pool", ErrAliasInvalid},
		{"b", "pool-1", ErrAliasInvalid},
		{"b", "node-1", nil},
	} {
		if err := r.SetAlias(tc.id, tc.alias); !errors.Is(err, tc.err) {
			t.Errorf("SetAlias(%q, %q) = %v, want %v", tc.id, tc.alias, err, tc.err)
		}
	}
	// Aliases survive re-registration and Forget, and can then be cleared.
	r.Register(proto.RegisterRequest{NodeID: "a"}, "y")
	if id, ok := r.Lookup("phone-01"); !ok || id != "a" || r.Snapshot()[0].Alias != "phone-01" {
		t.Fatalf("lookup after re-register: %s %v", id, ok)
	}
	_ = r.Forget("a")
	if id, ok := r.Lookup("phone-01"); !ok || id != "a" {
		t.Fatal("alias of forgotten node lost")
	}
	if err := r.SetAlias("a", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.SetAlias("a", ""); !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("clearing twice: %v", err)
	}
	if id, ok := r.Lookup("b"); !ok || id != "b" {
		t.Fatal("lookup by id")
	}
}

func TestNormPool(t *testing.T) {
	for _, tc := range []struct {
		p   Pool
		ok  bool
		out string
	}{
		{Pool{Name: "fast"}, true, "spread"},
		{Pool{Name: "fast", Routing: "affinity", Classes: []string{"s", "m", "s"}}, true, "affinity"},
		{Pool{Name: "Fast"}, false, ""},
		{Pool{Name: ""}, false, ""},
		{Pool{Name: "fast", Routing: "random"}, false, ""},
		{Pool{Name: "fast", Classes: []string{"huge"}}, false, ""},
		{Pool{Name: "fast", Models: []string{" "}}, false, ""},
		{Pool{Name: "fast", MinGenTPS: -1}, false, ""},
	} {
		got, err := normPool(tc.p)
		if (err == nil) != tc.ok || (tc.ok && got.Routing != tc.out) {
			t.Errorf("%+v: %+v %v", tc.p, got, err)
		}
	}
	if got, _ := normPool(Pool{Name: "x", Classes: []string{"s", "s"}, Members: []PoolMember{{}}}); len(got.Classes) != 1 || got.Members != nil || got.Models == nil {
		t.Errorf("not normalised: %+v", got)
	}
}

// poolNode builds a node for eligibility tests.
func poolNode(id, alias string, ram uint64, mod func(*proto.Node)) proto.Node {
	n := proto.Node{ID: id, Alias: alias, State: proto.StateActive, Inventory: proto.Inventory{RAMTotalBytes: ram},
		LastHeartbeat: &proto.Heartbeat{Runtime: &proto.RuntimeStatus{Model: "small", ModelID: "small", Ready: true, AdvertisePort: 1, GenTPS: 10}}}
	if mod != nil {
		mod(&n)
	}
	return n
}

func TestPoolMembers(t *testing.T) {
	hot := 80.0
	nodes := []proto.Node{
		poolNode("a", "phone-01", 4*models.GiB, nil),
		poolNode("b", "", 4*models.GiB, nil),
		poolNode("c", "", 8*models.GiB, nil), // class l
		poolNode("d", "", 4*models.GiB, nil), // drained
		poolNode("e", "", 4*models.GiB, func(n *proto.Node) { n.LastHeartbeat.TemperatureC = &hot }),
		poolNode("f", "", 4*models.GiB, func(n *proto.Node) { n.LastHeartbeat.Runtime.State, n.LastHeartbeat.Runtime.Ready = "loading", false }),
		poolNode("g", "", 4*models.GiB, func(n *proto.Node) { n.State = proto.StateSuspect }),
		poolNode("h", "", 4*models.GiB, func(n *proto.Node) { n.LastHeartbeat.Runtime.Model = "big" }),
		poolNode("i", "", 4*models.GiB, func(n *proto.Node) { n.LastHeartbeat.Runtime.GenTPS = 2 }),
		poolNode("j", "", 4*models.GiB, func(n *proto.Node) { n.LastHeartbeat = nil }),
	}
	drained := map[string]bool{"d": true}
	all := Pool{Name: "all"}
	strict := Pool{Name: "strict", Models: []string{"small"}, Classes: []string{"s"}, MinGenTPS: 5}
	members := Pool{Name: "members", Nodes: []string{"phone-01", "c"}}
	want := map[string][]string{ // reasons per node a..j
		"all":     {"", "", "", "drained", "hot", "switching", "not ready", "", "", "not ready"},
		"strict":  {"", "", "class not allowed", "drained", "hot", "switching", "not ready", "model not allowed", "below min_gen_tps", "not ready"},
		"members": {"", "not a member", "", "not a member", "not a member", "not a member", "not a member", "not a member", "not a member", "not a member"},
	}
	for _, p := range []Pool{all, strict, members} {
		got := poolMembers(p, nodes, drained, 75)
		for i, m := range got {
			if m.NodeID != nodes[i].ID || m.Reason != want[p.Name][i] || m.Eligible != (m.Reason == "") {
				t.Errorf("pool %s node %s: %+v, want reason %q", p.Name, nodes[i].ID, m, want[p.Name][i])
			}
		}
	}
	if m := poolMembers(all, nodes[:1], nil, 0)[0]; m.Alias != "phone-01" || m.Model != "small" {
		t.Errorf("member fields: %+v", m)
	}
	// Thermal limit 0 disables the hot check.
	if m := poolMembers(all, nodes[4:5], nil, 0)[0]; !m.Eligible {
		t.Errorf("hot with limit 0: %+v", m)
	}
}

// newRoutingEnv is newEnv with a routing file; nodes serve model "m".
func newRoutingEnv(t *testing.T, file string, nodes ...string) *env {
	t.Helper()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	st, err := LoadRouting(file)
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(time.Hour, time.Hour, log)
	srv := NewServer(reg, time.Second, GatewayOptions{Routing: RoutingOptions{File: file, State: st},
		Config: gateway.Config{UpstreamTimeout: 5 * time.Second, MaxAttempts: 2, Cooldown: time.Minute}},
		AdminOptions{Token: testToken}, log)
	e := &env{t: t, reg: reg, srv: srv, h: srv.Handler(), logs: logs}
	for _, id := range nodes {
		u, _ := url.Parse(fakeLlama(t).URL)
		port, _ := strconv.Atoi(u.Port())
		reg.Register(proto.RegisterRequest{NodeID: id, Inventory: proto.Inventory{RAMTotalBytes: 4 * models.GiB}}, "x")
		_ = reg.ReportBenchmark(proto.BenchmarkReport{NodeID: id})
		_ = reg.Heartbeat(proto.Heartbeat{NodeID: id, Runtime: &proto.RuntimeStatus{Model: "m", Ready: true, AdvertiseHost: u.Hostname(), AdvertisePort: port}})
	}
	return e
}

func TestAliasesAndPools(t *testing.T) {
	file := filepath.Join(t.TempDir(), "routing.json")
	e := newRoutingEnv(t, file, "a", "b", "c")

	var n AdminNode
	e.admin(http.MethodPatch, "/admin/nodes/a", `{"alias":"phone-01"}`, 200, &n)
	if n.ID != "a" || n.Alias != "phone-01" {
		t.Fatalf("patched node %+v", n)
	}
	e.admin(http.MethodPatch, "/admin/nodes/b", `{"alias":"phone-01"}`, 409, nil)
	e.admin(http.MethodPatch, "/admin/nodes/b", `{"alias":"auto"}`, 400, nil)
	e.admin(http.MethodPatch, "/admin/nodes/b", `{}`, 400, nil)
	e.admin(http.MethodPatch, "/admin/nodes/zz", `{"alias":"x"}`, 404, nil)
	if w := e.do(http.MethodGet, "/v1/nodes", "", ""); !strings.Contains(w.Body.String(), `"alias":"phone-01"`) {
		t.Fatalf("/v1/nodes lacks alias: %s", w.Body)
	}

	var p Pool
	e.admin(http.MethodPut, "/admin/pools/fast", `{"name":"ignored","description":"small agents","nodes":["phone-01","b"],"members":[{"node_id":"x"}]}`, 200, &p)
	if p.Name != "fast" || p.Routing != "spread" || len(p.Members) != 3 || !p.Members[0].Eligible || p.Members[2].Reason != ReasonNotMember {
		t.Fatalf("pool %+v", p)
	}
	e.admin(http.MethodPut, "/admin/pools/Bad", `{}`, 400, nil)
	e.admin(http.MethodPut, "/admin/pools/x", `{"routing":"random"}`, 400, nil)
	e.admin(http.MethodPut, "/admin/pools/x", `{"bogus":1}`, 400, nil)
	e.admin(http.MethodPut, "/admin/pools/sticky", `{"classes":["s"],"routing":"affinity"}`, 200, nil)

	// Requests to the pool reach only its members, spread over them.
	for i := 0; i < 4; i++ {
		w := e.do(http.MethodPost, "/v1/chat/completions", `{"model":"pool/fast","messages":[{"role":"system","content":"S"}]}`, "")
		if w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") == "c" {
			t.Fatalf("pool request: %d %s %s", w.Code, w.Header().Get("X-PhoneBorg-Node"), w.Body)
		}
	}
	if w := e.do(http.MethodPost, "/v1/chat/completions", `{"model":"node/phone-01"}`, ""); w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "a" {
		t.Fatalf("node request: %d %s", w.Code, w.Body)
	}
	if w := e.do(http.MethodPost, "/v1/chat/completions", `{"model":"node/c"}`, ""); w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "c" {
		t.Fatalf("node id request: %d %s", w.Code, w.Body)
	}
	e.admin(http.MethodPost, "/admin/nodes/a/drain", "", 200, nil)
	if w := e.do(http.MethodPost, "/v1/chat/completions", `{"model":"node/phone-01"}`, ""); w.Code != 503 || !strings.Contains(w.Body.String(), "node_unavailable") {
		t.Fatalf("drained node request: %d %s", w.Code, w.Body)
	}
	if w := e.do(http.MethodPost, "/v1/chat/completions", `{"model":"pool/nope"}`, ""); w.Code != 404 {
		t.Fatalf("unknown pool: %d", w.Code)
	}

	var pools Pools
	e.admin(http.MethodGet, "/admin/pools", "", 200, &pools)
	if len(pools.Pools) != 2 || pools.Pools[0].Name != "fast" || pools.Pools[0].Members[0].Reason != ReasonDrained {
		t.Fatalf("pools %+v", pools)
	}

	w := e.do(http.MethodGet, "/v1/models", "", "")
	for _, want := range []string{
		`{"id":"m","object":"model","owned_by":"phoneborg","kind":"model","nodes":2}`,
		`{"id":"auto","object":"model","owned_by":"phoneborg","kind":"auto","nodes":2}`,
		`{"id":"pool/fast","object":"model","owned_by":"phoneborg","kind":"pool","nodes":1,"description":"small agents"}`,
		`{"id":"pool/sticky","object":"model","owned_by":"phoneborg","kind":"pool","nodes":2}`,
		`{"id":"node/phone-01","object":"model","owned_by":"phoneborg","kind":"node","nodes":1,"model":"m","ready":false}`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("/v1/models lacks %s:\n%s", want, w.Body)
		}
	}
	metrics := e.do(http.MethodGet, "/metrics", "", "").Body.String()
	for _, want := range []string{
		`phoneborg_pool_members{pool="fast"} 1`,
		`phoneborg_pool_members{pool="sticky"} 2`,
		`phoneborg_gateway_target_requests_total{target="pool/fast"} 4`,
		`phoneborg_gateway_target_requests_total{target="node/phone-01"} 2`,
		`phoneborg_admin_actions_total{action="pool_set",result="ok"} 2`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	// Aliases and pools survive a restart.
	e2 := newRoutingEnv(t, file, "a")
	var nodes []AdminNode
	e2.admin(http.MethodGet, "/admin/nodes", "", 200, &nodes)
	e2.admin(http.MethodGet, "/admin/pools", "", 200, &pools)
	if nodes[0].Alias != "phone-01" || len(pools.Pools) != 2 || pools.Pools[0].Description != "small agents" {
		t.Fatalf("after restart: %+v %+v", nodes, pools)
	}

	e.admin(http.MethodDelete, "/admin/pools/fast", "", 204, nil)
	e.admin(http.MethodDelete, "/admin/pools/fast", "", 404, nil)
	e.admin(http.MethodPatch, "/admin/nodes/a", `{"alias":""}`, 200, &n)
	if n.Alias != "" {
		t.Fatalf("alias not cleared: %+v", n)
	}
	st, err := LoadRouting(file)
	if err != nil || len(st.Pools) != 1 || len(st.Aliases) != 0 {
		t.Fatalf("saved state %+v %v", st, err)
	}
}

func TestAdminPrewarm(t *testing.T) {
	e := newRoutingEnv(t, "", "a", "b")
	e.admin(http.MethodPatch, "/admin/nodes/a", `{"alias":"phone-01"}`, 200, nil)
	e.admin(http.MethodPut, "/admin/pools/fast", `{}`, 200, nil)
	msgs := `"messages":[{"role":"system","content":"You are terse."}]`

	var res PrewarmResponse
	e.admin(http.MethodPost, "/admin/prewarm", `{"target":"pool/fast",`+msgs+`,"tools":[]}`, 200, &res)
	if len(res.Results) != 2 || !res.Results[0].OK || res.Results[0].Alias != "phone-01" || res.Results[1].NodeID != "b" {
		t.Fatalf("results %+v", res)
	}
	e.admin(http.MethodPost, "/admin/prewarm", `{"target":"node/phone-01",`+msgs+`}`, 200, &res)
	if len(res.Results) != 1 || res.Results[0].NodeID != "a" {
		t.Fatalf("node results %+v", res)
	}
	e.admin(http.MethodPost, "/admin/prewarm", `{"target":"nope",`+msgs+`}`, 200, &res)
	if len(res.Results) != 0 {
		t.Fatalf("unserved model: %+v", res)
	}
	e.admin(http.MethodPost, "/admin/prewarm", `{"target":"pool/nope",`+msgs+`}`, 404, nil)
	e.admin(http.MethodPost, "/admin/prewarm", `{"target":"auto"}`, 400, nil)
	e.admin(http.MethodPost, "/admin/nodes/a/drain", "", 200, nil)
	e.admin(http.MethodPost, "/admin/prewarm", `{"target":"node/a",`+msgs+`}`, 503, nil)
	if w := e.do(http.MethodPost, "/admin/prewarm", `{"target":"auto",`+msgs+`}`, ""); w.Code != 401 {
		t.Fatalf("prewarm without token: %d", w.Code)
	}
	var out map[string]any
	e.admin(http.MethodPost, "/admin/prewarm", `{"target":"auto",`+msgs+`}`, 200, &out)
	if b, _ := json.Marshal(out); !strings.Contains(string(b), `"node_id":"b"`) || strings.Contains(string(b), `"node_id":"a"`) {
		t.Fatalf("auto prewarm %s", b)
	}
}
