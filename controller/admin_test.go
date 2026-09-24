package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/proto"
)

const testToken = "test-admin-token-0123456789"

// fakeLlama answers chat completions like llama-server.
func fakeLlama(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":8},"timings":{"cache_n":2,"predicted_per_second":4}}`)
	}))
	t.Cleanup(s.Close)
	return s
}

type env struct {
	t    *testing.T
	reg  *Registry
	srv  *Server
	h    http.Handler
	logs *bytes.Buffer
}

func newEnv(t *testing.T, token string, keys *gateway.StaticKeys, nodes ...string) *env {
	t.Helper()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reg := NewRegistry(time.Hour, time.Hour, log)
	srv := NewServer(reg, time.Second, GatewayOptions{Keys: keys,
		Config: gateway.Config{UpstreamTimeout: 5 * time.Second, MaxAttempts: 2, Cooldown: time.Minute}},
		AdminOptions{Token: token}, log)
	e := &env{t: t, reg: reg, srv: srv, h: srv.Handler(), logs: logs}
	for _, id := range nodes {
		u, _ := url.Parse(fakeLlama(t).URL)
		port, _ := strconv.Atoi(u.Port())
		reg.Register(proto.RegisterRequest{NodeID: id}, "x")
		_ = reg.ReportBenchmark(proto.BenchmarkReport{NodeID: id})
		_ = reg.Heartbeat(proto.Heartbeat{NodeID: id, Runtime: &proto.RuntimeStatus{Model: "m", Ready: true, AdvertiseHost: u.Hostname(), AdvertisePort: port}})
	}
	return e
}

func (e *env) do(method, path, body, bearer string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *env) admin(method, path, body string, want int, out any) {
	e.t.Helper()
	w := e.do(method, path, body, testToken)
	if w.Code != want {
		e.t.Fatalf("%s %s: status %d, want %d: %s", method, path, w.Code, want, w.Body)
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			e.t.Fatalf("%s %s: %v: %s", method, path, err, w.Body)
		}
	}
}

func (e *env) chat(key string) *httptest.ResponseRecorder {
	return e.do(http.MethodPost, "/v1/chat/completions", `{"model":"m","messages":[{"role":"system","content":"S"}]}`, key)
}

func TestAdminDisabledWithoutToken(t *testing.T) {
	e := newEnv(t, "", nil)
	for _, p := range []string{"/admin/nodes", "/admin/keys", "/admin/anything"} {
		w := e.do(http.MethodGet, p, "", "whatever")
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "-admin-token-file") {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	if w := e.do(http.MethodPost, "/admin/nodes/a/drain", "", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("drain: %d", w.Code)
	}
}

func TestAdminRequiresToken(t *testing.T) {
	e := newEnv(t, testToken, nil, "a")
	for _, tok := range []string{"", "wrong", testToken + "x", strings.ToUpper(testToken)} {
		for _, p := range []string{"/admin/nodes", "/admin/nope"} {
			if w := e.do(http.MethodGet, p, "", tok); w.Code != http.StatusUnauthorized {
				t.Fatalf("token %q %s: %d", tok, p, w.Code)
			}
		}
	}
	if w := e.do(http.MethodPost, "/admin/nodes/a/drain", "", "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("drain with wrong token: %d", w.Code)
	}
	if _, d := e.reg.View(); d["a"] {
		t.Fatal("unauthorized drain took effect")
	}
	e.admin(http.MethodGet, "/admin/nodes", "", 200, nil)
	metrics := e.do(http.MethodGet, "/metrics", "", "").Body.String()
	for _, want := range []string{
		`phoneborg_admin_actions_total{action="nodes_list",result="unauthorized"} 4`,
		`phoneborg_admin_actions_total{action="nodes_list",result="ok"} 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	if strings.Contains(e.logs.String(), testToken) || strings.Contains(metrics, testToken) {
		t.Fatal("admin token leaked into logs or metrics")
	}
}

func TestAdminDrainUndrainForget(t *testing.T) {
	e := newEnv(t, testToken, nil, "a")
	if w := e.chat(""); w.Code != 200 {
		t.Fatalf("chat: %d %s", w.Code, w.Body)
	}
	var nodes []AdminNode
	e.admin(http.MethodGet, "/admin/nodes", "", 200, &nodes)
	if len(nodes) != 1 || nodes[0].ID != "a" || nodes[0].Drained || nodes[0].PinnedSessions != 1 || nodes[0].LastHeartbeat.Runtime.Model != "m" {
		t.Fatalf("nodes = %+v", nodes)
	}

	var res struct {
		Drained       bool `json:"drained"`
		MovedSessions int  `json:"moved_sessions"`
	}
	e.admin(http.MethodPost, "/admin/nodes/a/drain", "", 200, &res)
	if !res.Drained || res.MovedSessions != 1 {
		t.Fatalf("drain = %+v", res)
	}
	e.admin(http.MethodGet, "/admin/nodes", "", 200, &nodes)
	if !nodes[0].Drained || nodes[0].PinnedSessions != 0 {
		t.Fatalf("after drain: %+v", nodes[0])
	}
	if w := e.chat(""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("chat to drained-only cluster: %d", w.Code)
	}
	if m := e.do(http.MethodGet, "/metrics", "", "").Body.String(); !strings.Contains(m, `phoneborg_node_drained{node_id="a"} 1`) {
		t.Fatal("drained metric missing")
	}
	if d := e.do(http.MethodGet, "/status", "", "").Body.String(); !strings.Contains(d, "DRAINED") {
		t.Fatal("dashboard does not show DRAINED")
	}

	e.admin(http.MethodPost, "/admin/nodes/a/undrain", "", 200, nil)
	if w := e.chat(""); w.Code != 200 {
		t.Fatalf("chat after undrain: %d", w.Code)
	}
	e.admin(http.MethodPost, "/admin/nodes/ghost/drain", "", 404, nil)

	e.admin(http.MethodDelete, "/admin/nodes/a", "", 204, nil)
	e.admin(http.MethodDelete, "/admin/nodes/a", "", 404, nil)
	e.admin(http.MethodGet, "/admin/nodes", "", 200, &nodes)
	if len(nodes) != 0 {
		t.Fatalf("forgotten node listed: %+v", nodes)
	}
	logs := e.logs.String()
	for _, want := range []string{`"action":"node_drain"`, `"action":"node_undrain"`, `"action":"node_forget"`, `"principal":"admin"`, `"node_id":"a"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("audit log lacks %s", want)
		}
	}
}

func TestAdminKeysLifecycleOpenToEnforced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys")
	keys := gateway.NewStaticKeys(path) // open store with a file, as if bootstrapping
	e := newEnv(t, testToken, keys, "a")

	e.admin(http.MethodPut, "/admin/gateway", `{"auth_mode":"keys"}`, http.StatusConflict, nil) // no keys yet

	var created CreatedKey
	e.admin(http.MethodPost, "/admin/keys", `{"name":"alice"}`, http.StatusCreated, &created)
	if !strings.HasPrefix(created.Key, "pb-") || created.AuthMode != "open" || !created.Persisted || len(created.Notes) != 1 {
		t.Fatalf("created = %+v", created)
	}
	e.admin(http.MethodPost, "/admin/keys", `{"name":"alice"}`, http.StatusConflict, nil)
	e.admin(http.MethodPost, "/admin/keys", `{"name":"bad name"}`, http.StatusBadRequest, nil)
	e.admin(http.MethodPost, "/admin/keys", `{"name":"x","extra":1}`, http.StatusBadRequest, nil)

	// Open: anonymous traffic still served, alice attributed.
	if w := e.chat(""); w.Code != 200 {
		t.Fatalf("open anonymous: %d", w.Code)
	}
	if w := e.chat(created.Key); w.Code != 200 {
		t.Fatalf("open alice: %d", w.Code)
	}

	var gs GatewaySettings
	e.admin(http.MethodPut, "/admin/gateway", `{"auth_mode":"keys"}`, 200, &gs)
	if gs.AuthMode != "keys" {
		t.Fatalf("settings = %+v", gs)
	}
	if w := e.chat(""); w.Code != http.StatusUnauthorized {
		t.Fatalf("enforced anonymous: %d", w.Code)
	}
	if w := e.chat(created.Key); w.Code != 200 {
		t.Fatalf("enforced alice: %d", w.Code)
	}
	e.admin(http.MethodPut, "/admin/gateway", `{"auth_mode":"open"}`, http.StatusConflict, nil)

	var list AdminKeys
	e.admin(http.MethodGet, "/admin/keys", "", 200, &list)
	if list.AuthMode != "keys" || len(list.Keys) != 1 || list.Keys[0].Name != "alice" || list.Keys[0].Usage.Requests != 2 ||
		list.Keys[0].Usage.CompletionTokens != 16 || list.Keys[0].Created.IsZero() {
		t.Fatalf("keys = %+v", list)
	}
	raw := e.do(http.MethodGet, "/admin/keys", "", testToken).Body.String()
	data, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if strings.Contains(raw, created.Key) || strings.Contains(raw, "sha256") || strings.Contains(string(data), created.Key) || st.Mode().Perm() != 0o600 {
		t.Fatalf("key material exposed: list=%s file(%v)=%s", raw, st.Mode(), data)
	}

	e.admin(http.MethodDelete, "/admin/keys/alice", "", 204, nil)
	e.admin(http.MethodDelete, "/admin/keys/alice", "", 404, nil)
	if w := e.chat(created.Key); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: %d", w.Code)
	}
	if strings.Contains(e.logs.String(), created.Key) {
		t.Fatal("API key leaked into logs")
	}
	if !strings.Contains(e.logs.String(), `"key_name":"alice"`) {
		t.Fatal("key actions not audited")
	}
}

func TestAdminGatewaySettings(t *testing.T) {
	e := newEnv(t, testToken, nil)
	var gs GatewaySettings
	e.admin(http.MethodGet, "/admin/gateway", "", 200, &gs)
	if gs != (GatewaySettings{Policy: "affinity", AffinitySpill: 2, UpstreamTimeout: "5s", AuthMode: "open"}) {
		t.Fatalf("defaults = %+v", gs)
	}
	for _, bad := range []string{
		`{"policy":"random"}`,
		`{"affinity_spill":0}`,
		`{"upstream_timeout":"soon"}`,
		`{"upstream_timeout":"10ms"}`,
		`{"policy":"least_inflight","upstream_timeout":"x"}`, // all-or-nothing
		`{"polcy":"affinity"}`,
		`{"auth_mode":"maybe"}`,
		`{"thermal_limit_c":-1}`,
		`{"thermal_limit_c":151}`,
	} {
		e.admin(http.MethodPut, "/admin/gateway", bad, http.StatusBadRequest, nil)
	}
	e.admin(http.MethodGet, "/admin/gateway", "", 200, &gs)
	if gs.Policy != "affinity" || gs.UpstreamTimeout != "5s" || gs.ThermalLimitC != 0 {
		t.Fatalf("rejected update changed settings: %+v", gs)
	}
	e.admin(http.MethodPut, "/admin/gateway", `{"policy":"least_inflight","affinity_spill":3,"upstream_timeout":"10m"}`, 200, &gs)
	if gs != (GatewaySettings{Policy: "least_inflight", AffinitySpill: 3, UpstreamTimeout: "10m0s", AuthMode: "open"}) {
		t.Fatalf("updated = %+v", gs)
	}
	if _, ok := e.srv.gw.Picker().(*gateway.LeastInflight); !ok || e.srv.gw.UpstreamTimeout() != 10*time.Minute {
		t.Fatal("settings not applied to the gateway")
	}

	e.admin(http.MethodPut, "/admin/gateway", `{"thermal_limit_c":60}`, 200, &gs)
	if gs.ThermalLimitC != 60 || e.srv.ThermalLimitC() != 60 {
		t.Fatalf("thermal limit not applied: %+v", gs)
	}
	e.admin(http.MethodPut, "/admin/gateway", `{"thermal_limit_c":0}`, 200, &gs)
	if gs.ThermalLimitC != 0 {
		t.Fatalf("thermal limit not disabled: %+v", gs)
	}
}

func TestAdminNodesReportsHot(t *testing.T) {
	e := newEnv(t, testToken, nil, "a")
	e.admin(http.MethodPut, "/admin/gateway", `{"thermal_limit_c":75}`, 200, nil)
	var nodes []AdminNode
	e.admin(http.MethodGet, "/admin/nodes", "", 200, &nodes)
	if len(nodes) != 1 || nodes[0].Hot {
		t.Fatalf("no heartbeat temperature: nodes = %+v", nodes)
	}
	hot := 80.0
	if err := e.reg.Heartbeat(proto.Heartbeat{NodeID: "a", TemperatureC: &hot,
		Runtime: &proto.RuntimeStatus{Model: "m", Ready: true, AdvertisePort: 4000}}); err != nil {
		t.Fatal(err)
	}
	e.admin(http.MethodGet, "/admin/nodes", "", 200, &nodes)
	if !nodes[0].Hot {
		t.Fatalf("expected node above thermal limit to be hot: %+v", nodes[0])
	}
	if m := e.do(http.MethodGet, "/metrics", "", "").Body.String(); !strings.Contains(m, `phoneborg_node_hot{node_id="a"} 1`) {
		t.Fatal("hot metric missing or wrong")
	}
}

func TestAdminStats(t *testing.T) {
	e := newEnv(t, testToken, nil, "a", "b")
	e.admin(http.MethodPost, "/admin/nodes/b/drain", "", 200, nil)
	for i := 0; i < 3; i++ {
		e.chat("")
	}
	var st Stats
	e.admin(http.MethodGet, "/admin/stats", "", 200, &st)
	c := st.Cluster
	if c.Nodes != 2 || c.ByState[proto.StateActive] != 2 || c.Drained != 1 || c.ReadyBackends != 1 || c.Models["m"] != 1 {
		t.Fatalf("cluster = %+v", c)
	}
	k := st.SinceStart.Keys[gateway.Anonymous]
	n := st.SinceStart.Nodes["a"]
	if k.Requests != 3 || k.PromptTokens != 15 || k.CachedPromptTokens != 6 || k.CompletionTokens != 24 || k.LastUsed.IsZero() {
		t.Fatalf("key usage = %+v", k)
	}
	if n.Requests != 3 || n.Errors != 0 || n.AvgGenTPS != 4 {
		t.Fatalf("node usage = %+v", n)
	}
	if st.Lifetime != nil || st.UptimeSeconds <= 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestLoadAdminToken(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		content, want string
		ok            bool
	}{
		{"# dev only\n\n  " + testToken + "  \n", testToken, true},
		{"short\n", "", false},
		{"# nothing\n", "", false},
	} {
		p := filepath.Join(dir, "tok")
		os.WriteFile(p, []byte(tc.content), 0o600)
		got, err := LoadAdminToken(p)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("%q: %q %v", tc.content, got, err)
		}
	}
	if _, err := LoadAdminToken(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestUsagePersistence(t *testing.T) {
	dir := t.TempDir()
	u, err := NewUsage(dir)
	if err != nil {
		t.Fatal(err)
	}
	u.Record(gateway.UsageEvent{Principal: "alice", NodeID: "a", Code: "200", PromptTokens: 10, CompletionTokens: 20, GenTPS: 10})
	u.Record(gateway.UsageEvent{Principal: "alice", NodeID: "b", Code: gateway.CodeUpstreamError})
	u.Record(gateway.UsageEvent{Principal: "alice", Code: "502"})
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Join(dir, "usage.json"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("usage.json mode %v", st.Mode())
	}

	again, err := NewUsage(dir)
	if err != nil {
		t.Fatal(err)
	}
	life, ok := again.Lifetime()
	if !ok || len(again.Session().Keys) != 0 {
		t.Fatal("lifetime missing or session not fresh")
	}
	k, a, b := life.Keys["alice"], life.Nodes["a"], life.Nodes["b"]
	// The retried attempt counts against node b, not as a client request.
	if k.Requests != 2 || k.Errors != 1 || k.CompletionTokens != 20 || a.Requests != 1 || a.AvgGenTPS != 10 || b.Requests != 1 || b.Errors != 1 {
		t.Fatalf("lifetime = %+v", life)
	}
	if !life.Since.Equal(u.Started()) {
		t.Fatalf("since %v, want first start %v", life.Since, u.Started())
	}

	os.WriteFile(filepath.Join(dir, "usage.json"), []byte("{broken"), 0o600)
	if _, err := NewUsage(dir); err == nil {
		t.Fatal("corrupt usage.json accepted")
	}
	if _, ok := (&Usage{}).Lifetime(); ok {
		t.Fatal("lifetime without state dir")
	}
}
