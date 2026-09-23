package controller

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

func newTestRegistry() (*Registry, *time.Time) {
	r := NewRegistry(15*time.Second, 30*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return now }
	return r, &now
}

func state(t *testing.T, r *Registry, id string) proto.NodeState {
	t.Helper()
	for _, n := range r.Snapshot() {
		if n.ID == id {
			return n.State
		}
	}
	t.Fatalf("node %s not found", id)
	return ""
}

func TestLifecycle(t *testing.T) {
	r, now := newTestRegistry()
	var transitions []string
	r.onTransition = func(_ string, from, to proto.NodeState) {
		transitions = append(transitions, string(from)+">"+string(to))
	}

	r.Register(proto.RegisterRequest{NodeID: "a", Inventory: proto.Inventory{RAMTotalBytes: 2 << 30}}, "x")
	if got := state(t, r, "a"); got != proto.StateBenchmarking {
		t.Fatalf("after register: %s", got)
	}
	if err := r.ReportBenchmark(proto.BenchmarkReport{NodeID: "a", Benchmark: proto.Benchmark{CPUGFLOPS: 1}}); err != nil {
		t.Fatal(err)
	}
	if got := state(t, r, "a"); got != proto.StateActive {
		t.Fatalf("after benchmark: %s", got)
	}

	*now = now.Add(16 * time.Second)
	r.Sweep()
	if got := state(t, r, "a"); got != proto.StateSuspect {
		t.Fatalf("after 16s silence: %s", got)
	}
	*now = now.Add(15 * time.Second)
	r.Sweep()
	if got := state(t, r, "a"); got != proto.StateOffline {
		t.Fatalf("after 31s silence: %s", got)
	}
	// Further sweeps must not bounce OFFLINE back to SUSPECT.
	r.Sweep()
	if got := state(t, r, "a"); got != proto.StateOffline {
		t.Fatalf("offline not sticky: %s", got)
	}

	if err := r.Heartbeat(proto.Heartbeat{NodeID: "a"}); err != nil {
		t.Fatal(err)
	}
	if got := state(t, r, "a"); got != proto.StateActive {
		t.Fatalf("after heartbeat resumed: %s", got)
	}

	want := []string{">BENCHMARKING", "BENCHMARKING>ACTIVE", "ACTIVE>SUSPECT", "SUSPECT>OFFLINE", "OFFLINE>ACTIVE"}
	if len(transitions) != len(want) {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}
	for i := range want {
		if transitions[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", transitions, want)
		}
	}
}

func TestUnknownNode(t *testing.T) {
	r, _ := newTestRegistry()
	if err := r.Heartbeat(proto.Heartbeat{NodeID: "ghost"}); err != ErrUnknownNode {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := r.ReportBenchmark(proto.BenchmarkReport{NodeID: "ghost"}); err != ErrUnknownNode {
		t.Fatalf("benchmark: %v", err)
	}
}

func TestReRegisterResetsBenchmark(t *testing.T) {
	r, _ := newTestRegistry()
	r.Register(proto.RegisterRequest{NodeID: "a"}, "x")
	_ = r.ReportBenchmark(proto.BenchmarkReport{NodeID: "a"})
	r.Register(proto.RegisterRequest{NodeID: "a"}, "x")
	n := r.Snapshot()[0]
	if n.State != proto.StateBenchmarking || n.Benchmark != nil {
		t.Fatalf("re-register: state=%s bench=%v", n.State, n.Benchmark)
	}
}

func TestDrainSurvivesReRegistrationAndForget(t *testing.T) {
	r, _ := newTestRegistry()
	if err := r.SetDrained("a", true); err != ErrUnknownNode {
		t.Fatalf("drain unknown node: %v", err)
	}
	r.Register(proto.RegisterRequest{NodeID: "a"}, "x")
	if err := r.SetDrained("a", true); err != nil {
		t.Fatal(err)
	}
	r.Register(proto.RegisterRequest{NodeID: "a"}, "x")
	if _, d := r.View(); !d["a"] {
		t.Fatal("drain lost on re-registration")
	}
	if err := r.Forget("a"); err != nil {
		t.Fatal(err)
	}
	if err := r.Heartbeat(proto.Heartbeat{NodeID: "a"}); err != ErrUnknownNode {
		t.Fatalf("heartbeat after forget: %v", err)
	}
	if err := r.Forget("a"); err != ErrUnknownNode {
		t.Fatalf("forget twice: %v", err)
	}
	nodes, d := r.View()
	if len(nodes) != 0 || !d["a"] {
		t.Fatalf("after forget: nodes=%v drained=%v", nodes, d)
	}
	// A forgotten node can still be undrained, once.
	if err := r.SetDrained("a", false); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDrained("a", false); err != ErrUnknownNode {
		t.Fatalf("undrain unknown: %v", err)
	}
}
