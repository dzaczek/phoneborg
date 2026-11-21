package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/proto"
)

// fakeStreamingLlama streams one token and llama.cpp timings.
func fakeStreamingLlama(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Paris\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"timings\":{\"cache_n\":3,\"prompt_n\":500,\"prompt_per_second\":15,\"predicted_n\":128,\"predicted_per_second\":7}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(s.Close)
	return s
}

// fakeAgent answers heartbeats like a node agent that loads whatever the
// controller asks for at once, serving it on llama.
func fakeAgent(t *testing.T, e *env, node string, llama *httptest.Server) {
	u, _ := url.Parse(llama.URL)
	port, _ := strconv.Atoi(u.Port())
	var stop atomic.Bool
	t.Cleanup(func() { stop.Store(true) })
	go func() {
		model := ""
		for !stop.Load() {
			rt := &proto.RuntimeStatus{Model: model, ModelID: model, Ready: model != "", State: "serving",
				AdvertiseHost: u.Hostname(), AdvertisePort: port, BudgetBytes: 6 << 30, CtxSize: 16384, KVType: "f16"}
			if _, d := e.heartbeat(proto.Heartbeat{NodeID: node, Runtime: rt}); d != nil {
				model = d.ModelID
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

func TestMMBBenchmarksEveryFittingModel(t *testing.T) {
	old := mmbPollEvery
	mmbPollEvery = 10 * time.Millisecond
	t.Cleanup(func() { mmbPollEvery = old })

	e, src := newModelEnv(t, map[string]float64{"p1": 8})
	second := filepath.Join(filepath.Dir(strings.TrimPrefix(src, "file://")), "Other-Model-Q4_K_M.gguf")
	data, _ := os.ReadFile(strings.TrimPrefix(src, "file://"))
	_ = os.WriteFile(second, data, 0o600)
	e.admin(http.MethodPost, "/admin/models", `{"source":"`+src+`"}`, http.StatusAccepted, nil)
	e.admin(http.MethodPost, "/admin/models", `{"source":"file://`+second+`"}`, http.StatusAccepted, nil)
	e.waitReady("qwen2.5-0.5b-instruct-q4_k_m")
	e.waitReady("other-model-q4_k_m")
	e.admin(http.MethodPatch, "/admin/nodes/p1", `{"alias":"pixel"}`, 200, nil)
	fakeAgent(t, e, "p1", fakeStreamingLlama(t))

	e.admin(http.MethodPost, "/admin/mmb", `{}`, 400, nil)
	e.admin(http.MethodPost, "/admin/mmb", `{"nodes":["ghost"]}`, 400, nil)
	var run MMBRun
	e.admin(http.MethodPost, "/admin/mmb", `{"nodes":["pixel"]}`, 201, &run)
	if len(run.Results) != 2 || run.Status != MMBRunning {
		t.Fatalf("run %+v", run)
	}
	for deadline := time.Now().Add(10 * time.Second); run.Status == MMBRunning; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("run did not finish: %+v", run)
		}
		e.admin(http.MethodGet, "/admin/mmb/"+run.ID, "", 200, &run)
	}
	if run.Status != MMBDone {
		t.Fatalf("status %s: %+v", run.Status, run)
	}
	for _, r := range run.Results {
		if r.Status != MMBDone || r.Cold == nil || r.Warm == nil || r.Short == nil || r.Cold.GenTPS != 7 || r.Cold.PromptTPS != 15 ||
			r.Warm.CachedTokens != 3 || r.Short.Text != "Paris" || r.CtxSize != 16384 || r.Alias != "pixel" {
			t.Errorf("result %+v", r)
		}
	}
	// The node is back: not drained, no benchmark override.
	_, drained := e.reg.View()
	if drained["p1"] {
		t.Error("node left drained")
	}
	if len(e.srv.place.overrides) != 0 {
		t.Errorf("override left: %v", e.srv.place.overrides)
	}
	var runs MMBRuns
	e.admin(http.MethodGet, "/admin/mmb", "", 200, &runs)
	if len(runs.Runs) != 1 {
		t.Errorf("runs %+v", runs)
	}
	e.admin(http.MethodDelete, "/admin/mmb/"+run.ID, "", 204, nil)
	if m := e.do(http.MethodGet, "/metrics", "", "").Body.String(); !strings.Contains(m, `phoneborg_mmb_results_total{status="done"} 2`) {
		t.Error("results not counted")
	}
}

func TestMMBWithOverrides(t *testing.T) {
	// A benchmark override replaces the node's pin while it lasts.
	spec := withOverrides(specWithPin("m1", "a", "b"), map[string]string{"a": "m2"})
	if len(spec.Policies) != 2 || spec.Policies[0].ModelID != "m2" || spec.Policies[0].Nodes[0] != "a" ||
		len(spec.Policies[1].Nodes) != 1 || spec.Policies[1].Nodes[0] != "b" {
		t.Errorf("spec %+v", spec)
	}
}

func specWithPin(model string, nodes ...string) models.Spec {
	return models.Spec{Policies: []models.Policy{{ModelID: model, Mode: models.ModePin, Nodes: nodes}}}
}

// modelsFor decides by the node's reported memory budget when it has one,
// and by the RAM class otherwise.
func TestMMBModelsForBudget(t *testing.T) {
	e, src := newModelEnv(t, map[string]float64{"xs1": 2.9, "xs2": 2.9})
	dir := filepath.Dir(strings.TrimPrefix(src, "file://"))
	small := strings.TrimPrefix(src, "file://")
	big := filepath.Join(dir, "Big-Model-Q4_K_M.gguf")
	data, _ := os.ReadFile(small)
	_ = os.WriteFile(big, data, 0o600)
	_ = os.Truncate(small, 400<<20)
	_ = os.Truncate(big, 800<<20)
	e.admin(http.MethodPost, "/admin/models", `{"source":"`+src+`"}`, http.StatusAccepted, nil)
	e.admin(http.MethodPost, "/admin/models", `{"source":"file://`+big+`"}`, http.StatusAccepted, nil)
	e.waitReady("qwen2.5-0.5b-instruct-q4_k_m")
	e.waitReady("big-model-q4_k_m")

	e.heartbeat(proto.Heartbeat{NodeID: "xs1", Runtime: &proto.RuntimeStatus{State: "serving", BudgetBytes: 640 << 20}})
	e.heartbeat(proto.Heartbeat{NodeID: "xs2", Runtime: &proto.RuntimeStatus{State: "serving"}})
	nodes := map[string]proto.Node{}
	all, _ := e.reg.View()
	for _, n := range all {
		nodes[n.ID] = n
	}
	want := []string{"qwen2.5-0.5b-instruct-q4_k_m", "big-model-q4_k_m"}

	got := e.srv.mmb.modelsFor(nodes["xs1"], want)
	if len(got) != 2 || got[0].Status != MMBQueued || got[1].Status != MMBSkipped ||
		!strings.Contains(got[1].Error, "even at the smallest context") || !strings.Contains(got[1].Error, "budget is 0.6 GiB") {
		t.Errorf("with a budget: %+v", got)
	}
	// Without a budget the class heuristic applies: nothing fits xs.
	for _, r := range e.srv.mmb.modelsFor(nodes["xs2"], want) {
		if r.Status != MMBSkipped || !strings.Contains(r.Error, "does not fit a xs phone") {
			t.Errorf("without a budget: %+v", r)
		}
	}
}
