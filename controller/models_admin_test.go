package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/controller/models/modeltest"
	"github.com/dzaczek/phoneborg/proto"
)

var ggufFile = modeltest.GGUF(modeltest.LlamaLike("qwen2", 24, 14, 2, 896, 32768), 1<<20)

// newModelEnv runs a controller with a catalog in a temp dir and nodes
// with the given RAM in GiB, each registered and benchmarked.
func newModelEnv(t *testing.T, ram map[string]float64) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cat, err := models.NewCatalog(models.CatalogOptions{Dir: filepath.Join(dir, "models"), StateFile: filepath.Join(dir, "models.json"), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cat.Close)
	reg := NewRegistry(time.Hour, time.Hour, log)
	srv := NewServer(reg, time.Second, GatewayOptions{
		Models: ModelOptions{Catalog: cat, PlacementFile: filepath.Join(dir, "placement.json")},
		Config: gateway.Config{UpstreamTimeout: 5 * time.Second, MaxAttempts: 2, Cooldown: time.Minute}},
		AdminOptions{Token: testToken}, log)
	e := &env{t: t, reg: reg, srv: srv, h: srv.Handler(), logs: logs}
	for id, gib := range ram {
		body, _ := json.Marshal(proto.RegisterRequest{NodeID: id, Inventory: proto.Inventory{RAMTotalBytes: uint64(gib * models.GiB)}})
		if w := e.do(http.MethodPost, proto.PathRegister, string(body), ""); w.Code != 200 {
			t.Fatalf("register %s: %d", id, w.Code)
		}
		_ = reg.ReportBenchmark(proto.BenchmarkReport{NodeID: id})
	}
	src := filepath.Join(dir, "Qwen2.5-0.5B-Instruct-Q4_K_M.gguf")
	os.WriteFile(src, ggufFile, 0o600)
	return e, "file://" + src
}

// heartbeat sends a heartbeat and returns the status and desired runtime.
func (e *env) heartbeat(hb proto.Heartbeat) (int, *proto.DesiredRuntime) {
	e.t.Helper()
	body, _ := json.Marshal(hb)
	w := e.do(http.MethodPost, proto.PathHeartbeat, string(body), "")
	if w.Code == http.StatusNoContent {
		return w.Code, nil
	}
	var resp proto.HeartbeatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		e.t.Fatalf("heartbeat reply %d: %v: %s", w.Code, err, w.Body)
	}
	return w.Code, resp.Desired
}

func (e *env) waitReady(id string) models.Model {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		var ms ModelList
		e.admin(http.MethodGet, "/admin/models", "", 200, &ms)
		for _, m := range ms.Models {
			if m.ID == id && m.Status != models.StatusDownloading {
				return m
			}
		}
	}
	e.t.Fatalf("model %s not ready", id)
	return models.Model{}
}

func TestModelLifecycleAndDesiredRuntime(t *testing.T) {
	e, src := newModelEnv(t, map[string]float64{"small": 2, "big": 6})
	const id = "qwen2.5-0.5b-instruct-q4_k_m"

	// Without a catalog entry nodes keep what pcprov gave them: 204.
	if code, d := e.heartbeat(proto.Heartbeat{NodeID: "big"}); code != http.StatusNoContent || d != nil {
		t.Fatalf("heartbeat before placement: %d %+v", code, d)
	}

	var m models.Model
	e.admin(http.MethodPost, "/admin/models", `{"source":"`+src+`","tags":["chat"],"recommended_classes":["m"]}`, http.StatusAccepted, &m)
	if m.ID != id || m.Status != models.StatusDownloading {
		t.Fatalf("added = %+v", m)
	}
	e.admin(http.MethodPost, "/admin/models", `{"source":"`+src+`"}`, http.StatusConflict, nil)
	e.admin(http.MethodPost, "/admin/models", `{"source":"ftp://x/y.gguf"}`, http.StatusBadRequest, nil)
	m = e.waitReady(id)
	h := sha256.Sum256(ggufFile)
	if m.Status != models.StatusReady || m.SHA256 != hex.EncodeToString(h[:]) || m.Arch != "qwen2" || m.HeadDim != 64 || m.Default {
		t.Fatalf("model = %+v", m)
	}

	var classes DeviceClasses
	e.admin(http.MethodGet, "/admin/device-classes", "", 200, &classes)
	if len(classes.Classes) != 5 || classes.Classes[0].ID != "xs" || classes.Classes[0].Nodes != 1 || classes.Classes[2].Nodes != 1 ||
		strings.Join(classes.Classes[2].RecommendedModels, ",") != id || classes.Classes[4].MaxRAMBytes != 0 {
		t.Fatalf("classes = %+v", classes)
	}

	// Default model: the 6 GiB node gets it, the 2 GiB one is too small.
	e.admin(http.MethodPatch, "/admin/models/"+id, `{"default":true}`, 200, &m)
	if !m.Default {
		t.Fatalf("patched = %+v", m)
	}
	code, d := e.heartbeat(proto.Heartbeat{NodeID: "big", Runtime: &proto.RuntimeStatus{Model: "old", Ready: true, AdvertisePort: 1}})
	want := proto.DesiredRuntime{ModelID: id, URL: "/v1/model-files/" + id, SHA256: m.SHA256, SizeBytes: m.SizeBytes,
		CtxSize: 16384, Slots: 1, KVType: "auto", Layers: 24, KVHeads: 2, HeadDim: 64}
	if code != http.StatusOK || d == nil || *d != want {
		t.Fatalf("heartbeat: %d %+v\nwant %+v", code, d, want)
	}
	if code, _ := e.heartbeat(proto.Heartbeat{NodeID: "small"}); code != http.StatusNoContent {
		t.Fatalf("small node: %d", code)
	}
	var pl Placement
	e.admin(http.MethodGet, "/admin/placement", "", 200, &pl)
	if pl.DefaultModel != id || len(pl.Plan) != 2 || pl.Plan[0].NodeID != "big" || pl.Plan[0].Reason != "default" ||
		pl.Plan[1].Reason != "none" || len(pl.Warnings) != 1 || !strings.Contains(pl.Warnings[0], "does not fit on small") {
		t.Fatalf("placement = %+v", pl)
	}

	// The node switches: not routable while loading, then serving.
	e.heartbeat(proto.Heartbeat{NodeID: "big", Runtime: &proto.RuntimeStatus{Model: "old", ModelID: id, State: "loading", Ready: true, AdvertisePort: 1}})
	if w := e.chat(""); w.Code != http.StatusServiceUnavailable && w.Code != http.StatusNotFound {
		t.Fatalf("chat while loading: %d %s", w.Code, w.Body)
	}
	e.heartbeat(proto.Heartbeat{NodeID: "big", Runtime: &proto.RuntimeStatus{Model: id, ModelID: id, State: "serving", Ready: true, AdvertisePort: 1}})
	var ms ModelList
	e.admin(http.MethodGet, "/admin/models", "", 200, &ms)
	if ms.Models[0].NodesServing != 1 {
		t.Fatalf("nodes_serving = %+v", ms.Models[0])
	}
	metrics := e.do(http.MethodGet, "/metrics", "", "").Body.String()
	for _, w := range []string{
		`phoneborg_model_info{arch="qwen2",model_id="` + id + `",params="1.5B",quant="Q4_K_M"} 1`,
		`phoneborg_model_download_progress{model_id="` + id + `"} 1`,
		`phoneborg_node_model{model_id="` + id + `",node_id="big",state="serving"} 1`,
		`phoneborg_placement_plan_nodes{model_id="` + id + `"} 1`,
	} {
		if !strings.Contains(metrics, w) {
			t.Errorf("metrics lack %s", w)
		}
	}

	// Referenced models cannot be deleted.
	e.admin(http.MethodDelete, "/admin/models/"+id, "", http.StatusConflict, nil)
	e.admin(http.MethodPatch, "/admin/models/"+id, `{"default":false,"tags":[]}`, 200, &m)
	if m.Default || len(m.Tags) != 0 {
		t.Fatalf("patched = %+v", m)
	}
	// Without an assignment the node keeps serving: 204 again.
	if code, _ := e.heartbeat(proto.Heartbeat{NodeID: "big"}); code != http.StatusNoContent {
		t.Fatalf("after unset: %d", code)
	}
	e.admin(http.MethodDelete, "/admin/models/"+id, "", http.StatusNoContent, nil)
	e.admin(http.MethodDelete, "/admin/models/"+id, "", http.StatusNotFound, nil)
	e.admin(http.MethodPatch, "/admin/models/"+id, `{"name":"x"}`, http.StatusNotFound, nil)

	logs := e.logs.String()
	for _, w := range []string{`"action":"model_add"`, `"action":"model_update"`, `"action":"model_delete"`, `"model_id":"` + id + `"`, `"msg":"placement changed"`} {
		if !strings.Contains(logs, w) {
			t.Errorf("logs lack %s", w)
		}
	}
}

func TestPlacementPutPreviewAndPersistence(t *testing.T) {
	e, src := newModelEnv(t, map[string]float64{"a": 4, "b": 6, "c": 8})
	const id = "qwen2.5-0.5b-instruct-q4_k_m"
	e.admin(http.MethodPost, "/admin/models", `{"source":"`+src+`"}`, http.StatusAccepted, nil)

	for _, bad := range []string{
		`{"policies":[{"model_id":"nope","mode":"replicas","replicas":1}]}`,
		`{"policies":[{"model_id":"` + id + `","mode":"pin","nodes":["ghost"]}]}`,
		`{"policies":[{"model_id":"` + id + `","mode":"percent","percent":120}]}`,
		`{"default_model":"nope"}`,
		`{"policies":[],"extra":1}`,
	} {
		e.admin(http.MethodPut, "/admin/placement", bad, http.StatusBadRequest, nil)
		e.admin(http.MethodPost, "/admin/placement/preview", bad, http.StatusBadRequest, nil)
	}

	// Policies for a model that is still downloading are accepted and wait.
	body := `{"policies":[{"model_id":"` + id + `","mode":"replicas","replicas":2,"classes":["m","l"]}]}`
	var preview Placement
	e.admin(http.MethodPost, "/admin/placement/preview", body, 200, &preview)
	e.waitReady(id)
	e.admin(http.MethodPost, "/admin/placement/preview", body, 200, &preview)
	if len(preview.Policies) != 1 || preview.Policies[0].Nodes == nil || len(preview.Plan) != 3 ||
		preview.Plan[0].Reason != "none" || preview.Plan[1].Reason != "replicas" || preview.Plan[2].Reason != "replicas" {
		t.Fatalf("preview = %+v", preview)
	}
	var cur Placement
	e.admin(http.MethodGet, "/admin/placement", "", 200, &cur)
	if len(cur.Policies) != 0 || cur.Plan[1].Reason != "none" {
		t.Fatalf("preview was applied: %+v", cur)
	}
	if code, _ := e.heartbeat(proto.Heartbeat{NodeID: "b"}); code != http.StatusNoContent {
		t.Fatalf("preview reached a node: %d", code)
	}

	e.admin(http.MethodPut, "/admin/placement", body, 200, &cur)
	if len(cur.Policies) != 1 || cur.Plan[1].ModelID != id || len(cur.Nodes) != 3 || cur.Nodes[0].Class != "s" || cur.Nodes[0].State != "idle" {
		t.Fatalf("applied = %+v", cur)
	}
	if code, d := e.heartbeat(proto.Heartbeat{NodeID: "c"}); code != 200 || d.ModelID != id {
		t.Fatalf("heartbeat c: %d %+v", code, d)
	}
	e.admin(http.MethodDelete, "/admin/models/"+id, "", http.StatusConflict, nil)

	// Persisted for the next controller.
	spec, err := LoadPlacement(filepath.Join(filepath.Dir(e.srv.catalog.Dir()), "placement.json"))
	if err != nil || len(spec.Policies) != 1 || spec.Policies[0].Replicas != 2 {
		t.Fatalf("persisted = %+v %v", spec, err)
	}
}

func TestModelFileDownloadWithRange(t *testing.T) {
	e, src := newModelEnv(t, nil)
	const id = "qwen2.5-0.5b-instruct-q4_k_m"
	if w := e.do(http.MethodGet, "/v1/model-files/"+id, "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown model: %d", w.Code)
	}
	e.admin(http.MethodPost, "/admin/models", `{"source":"`+src+`"}`, http.StatusAccepted, nil)
	m := e.waitReady(id)

	ts := httptest.NewServer(e.h)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/model-files/" + id) // no token needed
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, ggufFile) || resp.Header.Get("ETag") != `"`+m.SHA256+`"` ||
		resp.ContentLength != int64(len(ggufFile)) {
		t.Fatalf("full: %d %d bytes etag %s", resp.StatusCode, len(body), resp.Header.Get("ETag"))
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/model-files/"+id, nil)
	req.Header.Set("Range", "bytes=1000-")
	req.Header.Set("If-Range", `"`+m.SHA256+`"`)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, ggufFile[1000:]) {
		t.Fatalf("range: %d %d bytes", resp.StatusCode, len(body))
	}
}

func TestBackendsSkipNodesSwitchingModels(t *testing.T) {
	rt := func(state string, ready bool) *proto.Heartbeat {
		return &proto.Heartbeat{Runtime: &proto.RuntimeStatus{Model: "m", ModelID: "m", State: state, Ready: ready, AdvertisePort: 4000}}
	}
	nodes := []proto.Node{
		{ID: "serving", State: proto.StateActive, LastHeartbeat: rt("serving", true)},
		{ID: "legacy", State: proto.StateActive, LastHeartbeat: rt("", true)}, // agent without model management
		{ID: "downloading", State: proto.StateActive, LastHeartbeat: rt("downloading", true)},
		{ID: "loading", State: proto.StateActive, LastHeartbeat: rt("loading", false)},
		{ID: "error", State: proto.StateActive, LastHeartbeat: rt("error", false)},
	}
	got := Backends(nodes, nil, "h", 0)
	if len(got) != 2 || got[0].NodeID != "serving" || got[1].NodeID != "legacy" || got[0].Model != "m" {
		t.Fatalf("backends = %+v", got)
	}
}

func TestModelAdminRequiresToken(t *testing.T) {
	e, _ := newModelEnv(t, nil)
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/admin/models"}, {http.MethodPost, "/admin/models"}, {http.MethodPatch, "/admin/models/x"},
		{http.MethodDelete, "/admin/models/x"}, {http.MethodGet, "/admin/device-classes"}, {http.MethodGet, "/admin/placement"},
		{http.MethodPut, "/admin/placement"}, {http.MethodPost, "/admin/placement/preview"},
	} {
		if w := e.do(rt.method, rt.path, "{}", "wrong"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d", rt.method, rt.path, w.Code)
		}
	}
}
