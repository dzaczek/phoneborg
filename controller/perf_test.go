package controller

import (
	"math"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/proto"
)

func TestNodePerfUpdateKeepsLastGood(t *testing.T) {
	p := newNodePerf()

	// No runtime, or no model file size yet: nothing to compute from.
	if p.update("a", nil) {
		t.Fatal("update(nil) reported a change")
	}
	if p.update("a", &proto.RuntimeStatus{GenTPS: 10}) {
		t.Fatal("update with no ModelBytes reported a change")
	}
	if got := p.get("a"); got != (models.Perf{}) {
		t.Fatalf("get before any measurement = %+v", got)
	}

	// A fresh self-test records both directions.
	if !p.update("a", &proto.RuntimeStatus{GenTPS: 10, PromptTPS: 20, ModelBytes: 1_000_000_000}) {
		t.Fatal("first measurement did not report a change")
	}
	want := models.Perf{GenGBps: 10, PromptGBps: 20}
	if got := p.get("a"); got != want {
		t.Fatalf("get = %+v, want %+v", got, want)
	}

	// The identical measurement again is not a change.
	if p.update("a", &proto.RuntimeStatus{GenTPS: 10, PromptTPS: 20, ModelBytes: 1_000_000_000}) {
		t.Fatal("repeated measurement reported a change")
	}

	// A heartbeat mid model-switch (self-test not completed yet: GenTPS and
	// PromptTPS are 0) must not clear the last known-good value.
	if p.update("a", &proto.RuntimeStatus{ModelBytes: 1_000_000_000}) {
		t.Fatal("a zero self-test reported a change")
	}
	if got := p.get("a"); got != want {
		t.Fatalf("get after a stale heartbeat = %+v, want unchanged %+v", got, want)
	}

	// A new measurement updates just the field that changed.
	if !p.update("a", &proto.RuntimeStatus{GenTPS: 5, ModelBytes: 1_000_000_000}) {
		t.Fatal("a changed GenTPS did not report a change")
	}
	want.GenGBps = 5
	if got := p.get("a"); got != want {
		t.Fatalf("get after gen-only update = %+v, want %+v", got, want)
	}

	// snapshot/load round-trip.
	snap := p.snapshot()
	p2 := newNodePerf()
	p2.load(snap)
	if got := p2.get("a"); got != want {
		t.Fatalf("after load = %+v, want %+v", got, want)
	}
	if got := p2.get("unknown"); got != (models.Perf{}) {
		t.Fatalf("get unknown node = %+v", got)
	}
}

// TestNodePerfUpdateUsesResidentBytes covers ADR-012's addendum: bandwidth is
// computed from RuntimeStatus.ResidentBytes when the agent reports one,
// falling back to ModelBytes (the file size) when it does not (static mode,
// or an older agent).
func TestNodePerfUpdateUsesResidentBytes(t *testing.T) {
	p := newNodePerf()

	// Gemma 3n E2B-like numbers (docs/REAL_PHONES.md, ADR-012 addendum): a
	// 2886 MiB file but only 1446 MiB resident.
	const fileBytes, residentBytes = 2886 << 20, 1446 << 20
	if !p.update("a", &proto.RuntimeStatus{GenTPS: 3.7, ModelBytes: fileBytes, ResidentBytes: residentBytes}) {
		t.Fatal("update did not report a change")
	}
	want := models.Perf{GenGBps: models.Bandwidth(3.7, residentBytes)}
	got := p.get("a")
	if got != want {
		t.Fatalf("get = %+v, want %+v (from resident bytes)", got, want)
	}
	if bySize := models.Bandwidth(3.7, fileBytes); got.GenGBps == bySize {
		t.Fatalf("GenGBps used ModelBytes instead of ResidentBytes")
	}

	// No ResidentBytes reported (0): falls back to ModelBytes, like before.
	if !p.update("b", &proto.RuntimeStatus{GenTPS: 3.7, ModelBytes: fileBytes}) {
		t.Fatal("update did not report a change")
	}
	if want := (models.Perf{GenGBps: models.Bandwidth(3.7, fileBytes)}); p.get("b") != want {
		t.Fatalf("get = %+v, want %+v (fall back to ModelBytes)", p.get("b"), want)
	}
}

// TestNodePerfReportedAndPersisted covers the wiring between a real
// heartbeat and the controller's node-perf store: /admin/nodes and
// /admin/device-classes reflect the measured bandwidth (ADR-015), and it
// survives a controller restart via the routing file, like aliases and pools.
func TestNodePerfReportedAndPersisted(t *testing.T) {
	file := filepath.Join(t.TempDir(), "routing.json")
	e := newRoutingEnv(t, file, "mi8")

	// Mi 8 numbers (docs/DECISIONS.md ADR-015): Qwen2.5-1.5B, 1065 MiB, 6.8
	// gen tok/s, 11.4 prompt tok/s.
	const fileBytes = 1065 << 20
	wantGenGBps := models.Bandwidth(6.8, fileBytes)
	wantPromptGBps := models.Bandwidth(11.4, fileBytes)
	code, _ := e.heartbeat(proto.Heartbeat{NodeID: "mi8",
		Runtime: &proto.RuntimeStatus{Model: "m", Ready: true, GenTPS: 6.8, PromptTPS: 11.4, ModelBytes: fileBytes}})
	if code != http.StatusNoContent {
		t.Fatalf("heartbeat: %d", code)
	}

	var nodes []AdminNode
	e.admin(http.MethodGet, "/admin/nodes", "", 200, &nodes)
	if len(nodes) != 1 || nodes[0].Class != "s" || nodes[0].PerfTier != "t2" ||
		!closeEnough(nodes[0].GenGBps, wantGenGBps) || !closeEnough(nodes[0].PromptGBps, wantPromptGBps) {
		t.Fatalf("node = %+v, want gen %.4f prompt %.4f", nodes[0], wantGenGBps, wantPromptGBps)
	}

	var dc DeviceClasses
	e.admin(http.MethodGet, "/admin/device-classes", "", 200, &dc)
	tier := map[string]PerfTierCount{}
	for _, tc := range dc.PerfTiers {
		tier[tc.ID] = tc
	}
	if tier["t2"].Nodes != 1 || tier["t1"].Nodes != 0 || tier["t3"].Nodes != 0 || tier["t4"].Nodes != 0 {
		t.Fatalf("perf_tiers = %+v", dc.PerfTiers)
	}

	// A restart (new Server sharing the routing file) keeps the measurement.
	e2 := newRoutingEnv(t, file, "mi8")
	var nodes2 []AdminNode
	e2.admin(http.MethodGet, "/admin/nodes", "", 200, &nodes2)
	if len(nodes2) != 1 || nodes2[0].PerfTier != "t2" || !closeEnough(nodes2[0].GenGBps, wantGenGBps) {
		t.Fatalf("after restart: %+v", nodes2[0])
	}
}

func closeEnough(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
