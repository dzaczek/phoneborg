package controller

import (
	"bytes"
	"context"
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
	"sync"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/proto"
)

// fakeExt is an OpenAI-compatible server like LM Studio or Ollama's /v1.
type fakeExt struct {
	*httptest.Server
	mu      sync.Mutex
	models  []string
	down    bool // /v1/models answers 500
	fail    bool // chat answers 500
	timings bool // chat answers with llama.cpp timings
	auth    []string
	sent    []string      // "model" of each chat request
	block   chan struct{} // chat waits for it when set
	got     chan struct{} // signalled when a chat request arrives
}

func newFakeExt(t *testing.T, models ...string) *fakeExt {
	f := &fakeExt{models: models, got: make(chan struct{}, 16)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down, fail, timings, block := f.down, f.fail, f.timings, f.block
		if r.URL.Path == "/v1/models" {
			data := []map[string]string{}
			for _, m := range f.models {
				data = append(data, map[string]string{"id": m, "object": "model"})
			}
			f.mu.Unlock()
			if down {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.sent = append(f.sent, body.Model)
		f.mu.Unlock()
		f.got <- struct{}{}
		if block != nil {
			<-block
		}
		if fail {
			http.Error(w, "upstream broke", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if timings {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"1 2 3"}}],"usage":{"prompt_tokens":20,"completion_tokens":32},"timings":{"prompt_per_second":400,"predicted_per_second":55.5}}`)
			return
		}
		fmt.Fprintf(w, `{"model":%q,"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":8}}`, body.Model)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeExt) set(fn func(f *fakeExt)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeExt) seen() (auth, sent []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...), append([]string(nil), f.sent...)
}

// newExtEnv is newEnv with an external nodes file and a routing file;
// phones serve model "m".
func newExtEnv(t *testing.T, dir string, keys *gateway.StaticKeys, phones ...string) *env {
	t.Helper()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(&lockedWriter{w: logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var ext ExternalOptions
	if dir != "" {
		ext.File = filepath.Join(dir, "external.json")
		var err error
		if ext.State, err = LoadExternal(ext.File); err != nil {
			t.Fatal(err)
		}
	}
	reg := NewRegistry(time.Hour, time.Hour, log)
	srv := NewServer(reg, time.Second, GatewayOptions{Keys: keys, External: ext,
		Config: gateway.Config{UpstreamTimeout: 5 * time.Second, MaxAttempts: 2, Cooldown: time.Minute}},
		AdminOptions{Token: testToken}, log)
	e := &env{t: t, reg: reg, srv: srv, h: srv.Handler(), logs: logs}
	for _, id := range phones {
		u, _ := url.Parse(fakeLlama(t).URL)
		port, _ := strconv.Atoi(u.Port())
		reg.Register(proto.RegisterRequest{NodeID: id, Inventory: proto.Inventory{RAMTotalBytes: 4 * models.GiB}}, "x")
		_ = reg.ReportBenchmark(proto.BenchmarkReport{NodeID: id})
		_ = reg.Heartbeat(proto.Heartbeat{NodeID: id, Runtime: &proto.RuntimeStatus{Model: "m", Ready: true, AdvertiseHost: u.Hostname(), AdvertisePort: port}})
	}
	return e
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (e *env) chatModel(model, key string) *httptest.ResponseRecorder {
	return e.do(http.MethodPost, "/v1/chat/completions", `{"model":"`+model+`","messages":[{"role":"system","content":"S"}]}`, key)
}

func TestExternalPolling(t *testing.T) {
	f := newFakeExt(t, "qwen-7b", "llama-8b", "embed")
	e := newExtEnv(t, "", nil)
	var x External
	e.admin(http.MethodPut, "/admin/external/mac", `{"url":"`+f.URL+`/v1/"}`, 201, &x)
	if x.NodeID != "ext:mac" || x.URL != f.URL || x.State != ExternalActive || x.MaxConcurrency != 1 ||
		strings.Join(x.DiscoveredModels, ",") != "qwen-7b,llama-8b,embed" || x.LastCheck.IsZero() {
		t.Fatalf("external %+v", x)
	}
	// The allowlist filters and orders the discovered models.
	e.admin(http.MethodPut, "/admin/external/mac", `{"url":"`+f.URL+`","models":["llama-8b","qwen-7b","missing"]}`, 200, &x)
	if strings.Join(x.DiscoveredModels, ",") != "llama-8b,qwen-7b" || strings.Join(x.Models, ",") != "llama-8b,qwen-7b,missing" {
		t.Fatalf("allowlist %+v", x)
	}

	// Down: ACTIVE for two failed checks, OFFLINE on the third.
	f.set(func(f *fakeExt) { f.down = true })
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		e.srv.ext.poll(ctx, "mac")
		x, _ = e.srv.ext.get("mac", nil, nil)
		if want := map[bool]string{true: ExternalOffline, false: ExternalActive}[i == 3]; x.State != want || !strings.Contains(x.LastError, "HTTP 500") {
			t.Fatalf("check %d: %s %q", i, x.State, x.LastError)
		}
	}
	if len(e.srv.ext.backends()) != 0 {
		t.Fatal("OFFLINE external still routable")
	}
	f.set(func(f *fakeExt) { f.down = false })
	e.srv.ext.poll(ctx, "mac")
	if x, _ = e.srv.ext.get("mac", nil, nil); x.State != ExternalActive || x.LastError != "" {
		t.Fatalf("recovered %+v", x)
	}
	// Unreachable server.
	e.admin(http.MethodPut, "/admin/external/pc", `{"url":"http://127.0.0.1:1"}`, 201, &x)
	if x.State != ExternalOffline || x.LastError == "" {
		t.Fatalf("unreachable %+v", x)
	}

	metrics := e.do(http.MethodGet, "/metrics", "", "").Body.String()
	for _, want := range []string{
		`phoneborg_external_state_transitions_total{from="OFFLINE",to="ACTIVE"} 2`,
		`phoneborg_external_state_transitions_total{from="ACTIVE",to="OFFLINE"} 1`,
		`phoneborg_external_up{node_id="ext:mac"} 1`,
		`phoneborg_external_up{node_id="ext:pc"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	if !strings.Contains(e.logs.String(), `"msg":"external state transition","component":"external","name":"mac","node_id":"ext:mac","from":"ACTIVE","to":"OFFLINE"`) {
		t.Errorf("transition not logged:\n%s", e.logs)
	}

	// Validation and name rules.
	e.reg.Register(proto.RegisterRequest{NodeID: "phone-1"}, "x")
	_ = e.reg.SetAlias("phone-1", "fast")
	for _, tc := range []struct {
		name, body string
		code       int
	}{
		{"Mac", `{"url":"http://h:1"}`, 400},
		{"auto", `{"url":"http://h:1"}`, 400},
		{"pool-1", `{"url":"http://h:1"}`, 400},
		{"x", `{"url":"ftp://h"}`, 400},
		{"x", `{"url":"http://u:p@h:1"}`, 400},
		{"x", `{"url":"http://h:1","max_concurrency":-1}`, 400},
		{"x", `{"url":"http://h:1","models":[""]}`, 400},
		{"x", `{"url":"http://h:1","bogus":1}`, 400},
		{"fast", `{"url":"http://h:1"}`, 409},    // a phone's alias
		{"phone-1", `{"url":"http://h:1"}`, 409}, // a phone's id
	} {
		e.admin(http.MethodPut, "/admin/external/"+tc.name, tc.body, tc.code, nil)
	}
	e.admin(http.MethodPatch, "/admin/nodes/phone-1", `{"alias":"mac"}`, 409, nil)

	e.admin(http.MethodDelete, "/admin/external/pc", "", 204, nil)
	e.admin(http.MethodDelete, "/admin/external/pc", "", 404, nil)
	var list Externals
	e.admin(http.MethodGet, "/admin/external", "", 200, &list)
	if len(list.External) != 1 || list.External[0].Name != "mac" {
		t.Fatalf("list %+v", list)
	}
}

func TestExternalRouting(t *testing.T) {
	keys := gateway.NewStaticKeys("")
	clientKey, _, err := keys.Create("alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.Enforce(); err != nil {
		t.Fatal(err)
	}
	mac := newFakeExt(t, "qwen-7b", "llama-8b")
	e := newExtEnv(t, "", keys, "a")
	e.admin(http.MethodPut, "/admin/external/mac", `{"url":"`+mac.URL+`","api_key":"sk-upstream","ctx_size":32768}`, 201, nil)

	// A plain model id reaches the external, with its own key.
	w := e.chatModel("llama-8b", clientKey)
	if w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "ext:mac" {
		t.Fatalf("plain model: %d %s", w.Code, w.Body)
	}
	// node/<name> and node/ext:<name> go to the first model; "model" is rewritten.
	for _, target := range []string{"node/mac", "node/ext:mac"} {
		if w := e.chatModel(target, clientKey); w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "ext:mac" {
			t.Fatalf("%s: %d %s", target, w.Code, w.Body)
		}
	}
	auth, sent := mac.seen()
	if strings.Join(sent, ",") != "llama-8b,qwen-7b,qwen-7b" {
		t.Fatalf("upstream models %v", sent)
	}
	for _, a := range auth {
		if a != "Bearer sk-upstream" || strings.Contains(a, clientKey) {
			t.Fatalf("upstream Authorization %q", a)
		}
	}
	// Phones still serve their model.
	if w := e.chatModel("m", clientKey); w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "a" {
		t.Fatalf("phone: %d %s", w.Code, w.Body)
	}

	// A pool mixing a phone and the external, restricted by model.
	var p Pool
	e.admin(http.MethodPut, "/admin/pools/mixed", `{"nodes":["a","mac"],"models":["m","qwen-7b"]}`, 200, &p)
	if len(p.Members) != 3 || p.Members[1].NodeID != "ext:mac" || p.Members[1].Model != "qwen-7b" || !p.Members[1].Eligible ||
		p.Members[2].Model != "llama-8b" || p.Members[2].Reason != ReasonModel {
		t.Fatalf("pool members %+v", p.Members)
	}
	e.admin(http.MethodPut, "/admin/pools/extonly", `{"nodes":["ext:mac"],"models":["llama-8b"]}`, 200, nil)
	if w := e.chatModel("pool/extonly", clientKey); w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "ext:mac" {
		t.Fatalf("pool: %d %s", w.Code, w.Body)
	}
	if _, sent = mac.seen(); sent[len(sent)-1] != "llama-8b" {
		t.Fatalf("pool sent %v", sent)
	}
	e.admin(http.MethodPut, "/admin/pools/byclass", `{"classes":["m"]}`, 200, &p)
	for _, m := range p.Members {
		if m.NodeID == "ext:mac" && m.Reason != ReasonClass {
			t.Fatalf("class filter matched an external: %+v", m)
		}
	}

	// /v1/models lists the external's models and node/<name>.
	body := e.do(http.MethodGet, "/v1/models", "", clientKey).Body.String()
	for _, want := range []string{
		`{"id":"llama-8b","object":"model","owned_by":"phoneborg","kind":"model","nodes":1}`,
		`{"id":"qwen-7b","object":"model","owned_by":"phoneborg","kind":"model","nodes":1}`,
		`{"id":"node/mac","object":"model","owned_by":"phoneborg","kind":"node","nodes":1,"model":"qwen-7b","ready":true,"external":true}`,
		`{"id":"pool/mixed","object":"model","owned_by":"phoneborg","kind":"pool","nodes":2}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/v1/models lacks %s:\n%s", want, body)
		}
	}

	// Drain applies to externals too.
	e.admin(http.MethodPost, "/admin/nodes/ext:mac/drain", "", 200, nil)
	if w := e.chatModel("node/mac", clientKey); w.Code != 503 || !strings.Contains(w.Body.String(), "node_unavailable") {
		t.Fatalf("drained: %d %s", w.Code, w.Body)
	}
	e.admin(http.MethodPost, "/admin/nodes/ext:mac/undrain", "", 200, nil)
	e.admin(http.MethodPost, "/admin/nodes/ext:nope/drain", "", 404, nil)

	// Usage and per-node metrics use the ext: node id.
	var st Stats
	e.admin(http.MethodGet, "/admin/stats", "", 200, &st)
	if st.SinceStart.Nodes["ext:mac"].Requests != 4 || st.Cluster.External != 1 || st.Cluster.Models["qwen-7b"] != 1 || st.Cluster.Nodes != 1 {
		t.Fatalf("stats %+v", st)
	}
	metrics := e.do(http.MethodGet, "/metrics", "", "").Body.String()
	if !strings.Contains(metrics, `phoneborg_gateway_requests_total{code="200",model="qwen-7b",node_id="ext:mac",principal="alice"} 2`) {
		t.Errorf("metrics lack the external's requests:\n%s", metrics)
	}
	if strings.Contains(e.logs.String(), "sk-upstream") {
		t.Fatal("API key logged")
	}
}

func TestExternalConcurrencyLimit(t *testing.T) {
	mac := newFakeExt(t, "big")
	release := make(chan struct{})
	mac.set(func(f *fakeExt) { f.block = release })
	e := newExtEnv(t, "", nil)
	e.admin(http.MethodPut, "/admin/external/mac", `{"url":"`+mac.URL+`","max_concurrency":1}`, 201, nil)

	done := make(chan int)
	go func() { done <- e.chatModel("node/mac", "").Code }()
	<-mac.got
	if w := e.chatModel("node/mac", ""); w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"busy"`) {
		t.Fatalf("second request: %d %s", w.Code, w.Body)
	}
	if w := e.chatModel("big", ""); w.Code != 503 {
		t.Fatalf("plain model at the limit: %d %s", w.Code, w.Body)
	}
	close(release)
	if code := <-done; code != 200 {
		t.Fatalf("first request: %d", code)
	}
	if w := e.chatModel("node/mac", ""); w.Code != 200 {
		t.Fatalf("after release: %d %s", w.Code, w.Body)
	}
}

func TestExternalFailoverToPhone(t *testing.T) {
	mac := newFakeExt(t, "big")
	mac.set(func(f *fakeExt) { f.fail = true })
	e := newExtEnv(t, "", nil, "a")
	// The speed hint makes the external the first choice.
	e.admin(http.MethodPut, "/admin/external/mac", `{"url":"`+mac.URL+`","speed_tps":100}`, 201, nil)
	w := e.chatModel("auto", "")
	if w.Code != 200 || w.Header().Get("X-PhoneBorg-Node") != "a" {
		t.Fatalf("failover: %d %s %s", w.Code, w.Header().Get("X-PhoneBorg-Node"), w.Body)
	}
	if _, sent := mac.seen(); len(sent) != 1 {
		t.Fatalf("external not tried first: %v", sent)
	}
	// A node target never fails over.
	if w := e.chatModel("node/mac", ""); w.Code != 502 {
		t.Fatalf("node target: %d %s", w.Code, w.Body)
	}
}

func TestExternalReapedWhenOffline(t *testing.T) {
	mac := newFakeExt(t, "big")
	release := make(chan struct{})
	defer close(release)
	mac.set(func(f *fakeExt) { f.block = release })
	e := newExtEnv(t, "", nil, "a")
	e.admin(http.MethodPut, "/admin/external/mac", `{"url":"`+mac.URL+`"}`, 201, nil)
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- e.chatModel("node/mac", "") }()
	<-mac.got
	mac.set(func(f *fakeExt) { f.down = true })
	for range externalFailsOffline {
		e.srv.ext.poll(context.Background(), "mac")
	}
	e.srv.ReapGateway()
	if w := <-done; w.Code != 502 {
		t.Fatalf("reaped request: %d %s", w.Code, w.Body)
	}
}

func TestExternalPersistence(t *testing.T) {
	dir := t.TempDir()
	mac := newFakeExt(t, "big")
	e := newExtEnv(t, dir, nil)
	e.admin(http.MethodPut, "/admin/external/mac", `{"url":"`+mac.URL+`","api_key":"sk-secret","models":["big"],"max_concurrency":2,"ctx_size":8192,"speed_tps":40}`, 201, nil)
	w := e.do(http.MethodGet, "/admin/external", "", testToken)
	if w.Code != 200 || strings.Contains(w.Body.String(), "sk-secret") || !strings.Contains(w.Body.String(), `"has_api_key":true`) {
		t.Fatalf("GET: %d %s", w.Code, w.Body)
	}
	file := filepath.Join(dir, "external.json")
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v %v", fi, err)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), "sk-secret") {
		t.Fatalf("key not persisted: %s", data)
	}
	if strings.Contains(e.logs.String(), "sk-secret") {
		t.Fatal("API key logged")
	}

	// A PUT without api_key keeps the key; "" removes it.
	var x External
	e.admin(http.MethodPut, "/admin/external/mac", `{"url":"`+mac.URL+`"}`, 200, &x)
	if !x.HasAPIKey {
		t.Fatal("key dropped by a PUT without api_key")
	}

	// Restart: the config comes back, starting OFFLINE until checked.
	e2 := newExtEnv(t, dir, nil)
	var list Externals
	e2.admin(http.MethodGet, "/admin/external", "", 200, &list)
	if len(list.External) != 1 || !list.External[0].HasAPIKey || list.External[0].URL != mac.URL || list.External[0].State != ExternalOffline {
		t.Fatalf("after restart %+v", list)
	}
	e2.srv.ext.poll(context.Background(), "mac")
	if w := e2.chatModel("big", ""); w.Code != 200 {
		t.Fatalf("after restart: %d %s", w.Code, w.Body)
	}
	if auth, _ := mac.seen(); auth[len(auth)-1] != "Bearer sk-secret" {
		t.Fatalf("upstream auth %v", auth)
	}
	e2.admin(http.MethodPut, "/admin/external/mac", `{"url":"`+mac.URL+`","api_key":""}`, 200, &x)
	if data, _ := os.ReadFile(file); x.HasAPIKey || strings.Contains(string(data), "sk-secret") {
		t.Fatalf("key not removed: %+v %s", x, data)
	}
}

func TestExternalSelfTest(t *testing.T) {
	mac := newFakeExt(t, "a", "b")
	e := newExtEnv(t, "", nil)
	e.admin(http.MethodPut, "/admin/external/mac", `{"url":"`+mac.URL+`"}`, 201, nil)
	var x External
	e.admin(http.MethodPost, "/admin/external/mac/selftest", "", 200, &x)
	if len(x.Measured) != 2 || x.Measured["a"].GenTPS <= 0 || x.Measured["a"].PromptTPS != 0 || x.Measured["a"].Error != "" {
		t.Fatalf("wall-clock self-test %+v", x.Measured)
	}
	mac.set(func(f *fakeExt) { f.timings = true })
	e.admin(http.MethodPost, "/admin/external/mac/selftest", "", 200, &x)
	if x.Measured["b"].GenTPS != 55.5 || x.Measured["b"].PromptTPS != 400 {
		t.Fatalf("timings self-test %+v", x.Measured)
	}
	for _, b := range e.srv.ext.backends() {
		if b.Speed != 55.5 {
			t.Fatalf("backend speed %+v", b)
		}
	}
	// Automatic self-tests only cover models without a measurement.
	mac.set(func(f *fakeExt) { f.models = append(f.models, "c") })
	e.srv.ext.poll(context.Background(), "mac")
	_, before := mac.seen()
	if _, err := e.srv.ext.selfTest(context.Background(), "mac", false); err != nil {
		t.Fatal(err)
	}
	if _, after := mac.seen(); len(after) != len(before)+1 || after[len(after)-1] != "c" {
		t.Fatalf("auto self-test sent %v", after[len(before):])
	}
	// The operator's hint overrides measurements.
	e.admin(http.MethodPut, "/admin/external/mac", `{"url":"`+mac.URL+`","speed_tps":7}`, 200, nil)
	for _, b := range e.srv.ext.backends() {
		if b.Speed != 7 {
			t.Fatalf("hinted speed %+v", b)
		}
	}
	e.admin(http.MethodPost, "/admin/external/nope/selftest", "", 404, nil)
}

func TestPlannerIgnoresExternals(t *testing.T) {
	mac := newFakeExt(t, "big")
	e := newExtEnv(t, "", nil, "a")
	e.admin(http.MethodPut, "/admin/external/mac", `{"url":"`+mac.URL+`"}`, 201, nil)
	e.srv.Replan()
	var pl Placement
	e.admin(http.MethodGet, "/admin/placement", "", 200, &pl)
	if len(pl.Nodes) != 1 || pl.Nodes[0].NodeID != "a" {
		t.Fatalf("placement nodes %+v", pl.Nodes)
	}
	for _, a := range pl.Plan {
		if strings.HasPrefix(a.NodeID, ExternalPrefix) {
			t.Fatalf("external planned: %+v", a)
		}
	}
	var dc DeviceClasses
	e.admin(http.MethodGet, "/admin/device-classes", "", 200, &dc)
	total := 0
	for _, c := range dc.Classes {
		total += c.Nodes
	}
	if total != 1 {
		t.Fatalf("device classes count %d nodes: %+v", total, dc.Classes)
	}
	var nodes []AdminNode
	e.admin(http.MethodGet, "/admin/nodes", "", 200, &nodes)
	if len(nodes) != 1 {
		t.Fatalf("/admin/nodes %+v", nodes)
	}
}
