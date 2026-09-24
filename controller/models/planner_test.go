package models

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Test models: est RAM at 16k is file + KV + 150 MiB.
var (
	small = PlanModel{ID: "small", SizeBytes: 400 << 20, CtxTrain: 32768, Layers: 24, KVHeads: 2, HeadDim: 64}  // ~0.7 GiB
	mid   = PlanModel{ID: "mid", SizeBytes: 1 << 30, CtxTrain: 32768, Layers: 28, KVHeads: 2, HeadDim: 128}     // ~1.6 GiB
	big   = PlanModel{ID: "big", SizeBytes: 4000 << 20, CtxTrain: 131072, Layers: 36, KVHeads: 8, HeadDim: 128} // ~6.3 GiB
	short = PlanModel{ID: "short", SizeBytes: 100 << 20, CtxTrain: 2048, Layers: 12, KVHeads: 12, HeadDim: 64}
	all   = []PlanModel{small, mid, big, short}
)

func node(id string, ramGiB float64, speed float64, current string) Node {
	ram := uint64(ramGiB * GiB)
	return Node{ID: id, Class: ClassOf(ram), RAMTotalBytes: ram, Speed: speed, CurrentModel: current}
}

// summary renders a plan as "node=model/reason" in node order.
func summary(p Plan) string {
	var s []string
	for _, a := range p.Assignments {
		s = append(s, fmt.Sprintf("%s=%s/%s", a.NodeID, a.ModelID, a.Reason))
	}
	return strings.Join(s, " ")
}

func TestPlan(t *testing.T) {
	fleet := []Node{
		node("a", 4, 8, ""),         // s
		node("b", 5.5, 12, "small"), // m, serves small
		node("c", 7.5, 20, ""),      // l, fastest
		node("d", 11, 15, "legacy"), // xl, serves a model not in the catalog
		node("e", 2, 30, ""),        // xs: fits nothing at 16k
	}
	for _, tc := range []struct {
		name     string
		spec     Spec
		nodes    []Node
		want     string
		warnings []string
	}{
		{name: "no policies keeps what nodes serve", nodes: fleet,
			want: "a=/none b=small/keep c=/none d=legacy/keep e=/none"},
		{name: "pin wins and warns when it does not fit", nodes: fleet,
			spec: Spec{Policies: []Policy{{ModelID: "big", Mode: ModePin, Nodes: []string{"a", "d"}}}},
			want: "a=big/pin b=small/keep c=/none d=big/pin e=/none", warnings: []string{"big: pinned to a (4.0 GiB)"}},
		{name: "pin to a node that left", nodes: fleet,
			spec: Spec{Policies: []Policy{{ModelID: "small", Mode: ModePin, Nodes: []string{"gone"}}}},
			want: "a=/none b=small/keep c=/none d=legacy/keep e=/none", warnings: []string{"pinned node gone is not connected"}},
		{name: "replicas keep the node already serving, then pick the fastest", nodes: fleet,
			spec: Spec{Policies: []Policy{{ModelID: "small", Mode: ModeReplicas, Replicas: 2}}},
			want: "a=/none b=small/replicas c=small/replicas d=legacy/keep e=/none"},
		{name: "bigger model picks the fastest nodes first", nodes: fleet,
			spec: Spec{Policies: []Policy{{ModelID: "small", Mode: ModeReplicas, Replicas: 1}, {ModelID: "mid", Mode: ModeReplicas, Replicas: 1}}},
			want: "a=/none b=small/replicas c=mid/replicas d=legacy/keep e=/none"},
		{name: "replicas only on nodes where the model fits", nodes: fleet,
			spec: Spec{Policies: []Policy{{ModelID: "big", Mode: ModeReplicas, Replicas: 3}}},
			want: "a=/none b=small/keep c=/none d=big/replicas e=/none", warnings: []string{"big: wants 3 node(s) (replicas=3), only 1 eligible"}},
		{name: "class filter", nodes: fleet,
			spec: Spec{Policies: []Policy{{ModelID: "small", Mode: ModeReplicas, Replicas: 5, Classes: []string{"s", "xl"}}}},
			want: "a=small/replicas b=small/keep c=/none d=small/replicas e=/none", warnings: []string{"only 2 eligible"}},
		{name: "percent of eligible nodes rounds half up", nodes: fleet,
			// Eligible for small: a, b, c, d (e is too small). 50% of 4 = 2.
			spec: Spec{Policies: []Policy{{ModelID: "small", Mode: ModePercent, Percent: 50}}},
			want: "a=/none b=small/percent c=small/percent d=legacy/keep e=/none"},
		{name: "percent 12.5 of 4 is 0.5, rounds to 1", nodes: fleet,
			spec: Spec{Policies: []Policy{{ModelID: "mid", Mode: ModePercent, Percent: 12.5}}},
			want: "a=/none b=small/keep c=mid/percent d=legacy/keep e=/none"},
		{name: "tiny percent still gets one node", nodes: fleet,
			spec: Spec{Policies: []Policy{{ModelID: "mid", Mode: ModePercent, Percent: 1}}},
			want: "a=/none b=small/keep c=mid/percent d=legacy/keep e=/none"},
		{name: "percent after replicas takes what is left", nodes: fleet,
			spec: Spec{Policies: []Policy{{ModelID: "mid", Mode: ModePercent, Percent: 100}, {ModelID: "small", Mode: ModeReplicas, Replicas: 1}}},
			want: "a=mid/percent b=small/replicas c=mid/percent d=mid/percent e=/none", warnings: []string{"mid: wants 4 node(s) (percent=100), only 3"}},
		{name: "default fills unassigned nodes where it fits", nodes: fleet,
			spec: Spec{Policies: []Policy{{ModelID: "big", Mode: ModeReplicas, Replicas: 1}}, DefaultModel: "small"},
			want: "a=small/default b=small/default c=small/default d=big/replicas e=/none", warnings: []string{"default model small needs about 0.7 GiB and does not fit on e"}},
		{name: "drained nodes only follow pins", nodes: []Node{{ID: "x", Class: "m", RAMTotalBytes: 6 * GiB, Drained: true, CurrentModel: "small"}, node("y", 6, 1, "")},
			spec: Spec{Policies: []Policy{{ModelID: "mid", Mode: ModeReplicas, Replicas: 2}}, DefaultModel: "small"},
			want: "x=small/keep y=mid/replicas", warnings: []string{"mid: wants 2"}},
		{name: "policies for models that are not ready wait", nodes: fleet,
			spec:     Spec{Policies: []Policy{{ModelID: "downloading", Mode: ModeReplicas, Replicas: 1}}, DefaultModel: "downloading"},
			want:     "a=/none b=small/keep c=/none d=legacy/keep e=/none",
			warnings: []string{"downloading: model is not ready; its replicas policy waits", "default model downloading is not ready"}},
		{name: "previous assignment beats speed", nodes: []Node{node("slow", 6, 1, ""), {ID: "fast", Class: "m", RAMTotalBytes: 6 * GiB, Speed: 50}, {ID: "switching", Class: "m", RAMTotalBytes: 6 * GiB, Assigned: "mid"}},
			spec: Spec{Policies: []Policy{{ModelID: "mid", Mode: ModeReplicas, Replicas: 1}}},
			want: "fast=/none slow=/none switching=mid/replicas"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultPlanner{}.Plan(tc.spec, tc.nodes, all)
			if got := summary(p); got != tc.want {
				t.Errorf("plan:\n got %s\nwant %s", got, tc.want)
			}
			if len(p.Warnings) != len(tc.warnings) {
				t.Errorf("warnings = %q, want %d", p.Warnings, len(tc.warnings))
			}
			for i, w := range tc.warnings {
				if i < len(p.Warnings) && !strings.Contains(p.Warnings[i], w) {
					t.Errorf("warning %d = %q, want it to contain %q", i, p.Warnings[i], w)
				}
			}
			// Deterministic, and independent of input order.
			rev := make([]Node, len(tc.nodes))
			for i, n := range tc.nodes {
				rev[len(rev)-1-i] = n
			}
			if again := (DefaultPlanner{}).Plan(tc.spec, rev, all); !reflect.DeepEqual(again, p) {
				t.Errorf("plan depends on node order:\n%s\n%s", summary(p), summary(again))
			}
		})
	}
}

func TestPlanAssignmentDetails(t *testing.T) {
	p := DefaultPlanner{}.Plan(Spec{Policies: []Policy{{ModelID: "short", Mode: ModePin, Nodes: []string{"a"}}}},
		[]Node{node("a", 4, 0, "")}, all)
	a := p.Assignments[0]
	want := Assignment{NodeID: "a", ModelID: "short", Reason: ReasonPin, CtxSize: 2048, Slots: 1, KVType: "auto",
		EstRAMBytes: short.SizeBytes + KVCacheBytes(12, 12, 64, 2048) + RuntimeOverheadBytes, Fits: true}
	if a != want {
		t.Fatalf("assignment = %+v\nwant         %+v", a, want)
	}
	if p.Warnings == nil {
		t.Fatal("warnings must be [] not null")
	}
}

// budgetNode is a node reporting a memory budget (RuntimeStatus.BudgetBytes,
// ADR-012), the way a real agent's heartbeat does.
func budgetNode(id string, ramGiB float64, budgetMiB int64) Node {
	n := node(id, ramGiB, 0, "")
	n.BudgetBytes = budgetMiB << 20
	return n
}

// TestPlanNodeBudget covers the bug this fixes: the class heuristic (RAM
// minus a fixed 2 GiB Android baseline) found no room for Llama-3.2-1B on
// classes xs,s even though a real 3 GiB phone has some. Once a node reports
// its own measured budget, fit is checked against that instead.
func TestPlanNodeBudget(t *testing.T) {
	// 770 MiB file, 16 layers, 8 KV heads, head_dim 64 (Llama-3.2-1B's
	// shape, ADR-012). EstRAM() at the default 16k context is ~1.4 GiB.
	llama1b := PlanModel{ID: "llama-3.2-1b-instruct-q4_k_m", SizeBytes: 770 << 20, CtxTrain: 131072, Layers: 16, KVHeads: 8, HeadDim: 64}
	catalog := []PlanModel{llama1b}

	mi8 := budgetNode("mi8", 5.5, 2560) // real Xiaomi Mi 8: ~2.5 GiB budget
	emu3 := budgetNode("emu3", 3, 1740) // 3 GiB emulator: ~1.7 GiB budget
	emu2 := budgetNode("emu2", 2, 922)  // 2 GiB emulator: ~0.9 GiB budget

	for _, tc := range []struct {
		name     string
		nodes    []Node
		spec     Spec
		want     string
		warnings []string
	}{
		{name: "pin fits a 3 GiB phone once its real budget is known", nodes: []Node{emu3},
			spec: Spec{Policies: []Policy{{ModelID: llama1b.ID, Mode: ModePin, Nodes: []string{"emu3"}}}},
			want: "emu3=llama-3.2-1b-instruct-q4_k_m/pin"},
		{name: "pin still warns when the real budget is too small", nodes: []Node{emu2},
			spec:     Spec{Policies: []Policy{{ModelID: llama1b.ID, Mode: ModePin, Nodes: []string{"emu2"}}}},
			want:     "emu2=llama-3.2-1b-instruct-q4_k_m/pin",
			warnings: []string{"pinned to emu2"}},
		{name: "replicas skips a node whose budget is really too small", nodes: []Node{emu2, emu3, mi8},
			spec:     Spec{Policies: []Policy{{ModelID: llama1b.ID, Mode: ModeReplicas, Replicas: 3}}},
			want:     "emu2=/none emu3=llama-3.2-1b-instruct-q4_k_m/replicas mi8=llama-3.2-1b-instruct-q4_k_m/replicas",
			warnings: []string{"wants 3 node(s)"}},
		{
			// The observed bug: percent of classes xs,s got 0 nodes because
			// the heuristic rejected both the 2 GiB and the 3 GiB phone.
			// With real budgets, the 3 GiB phone fits.
			name: "percent of classes xs,s now places on the 3 GiB phone", nodes: []Node{emu2, emu3},
			spec: Spec{Policies: []Policy{{ModelID: llama1b.ID, Mode: ModePercent, Percent: 50, Classes: []string{"xs", "s"}}}},
			want: "emu2=/none emu3=llama-3.2-1b-instruct-q4_k_m/percent"},
		{
			// Restricted to xs only (the 2 GiB phone), nothing fits even
			// with its real budget: this must still warn, not silently
			// place nothing (the other half of the observed bug: percentOf
			// a zero-sized pool is 0, so "got < want" never fired).
			name: "percent with no fit still warns instead of placing nothing silently", nodes: []Node{emu2, emu3},
			spec:     Spec{Policies: []Policy{{ModelID: llama1b.ID, Mode: ModePercent, Percent: 50, Classes: []string{"xs"}}}},
			want:     "emu2=/none emu3=/none",
			warnings: []string{"none has enough memory"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultPlanner{}.Plan(tc.spec, tc.nodes, catalog)
			if got := summary(p); got != tc.want {
				t.Errorf("plan:\n got %s\nwant %s", got, tc.want)
			}
			if len(p.Warnings) != len(tc.warnings) {
				t.Fatalf("warnings = %q, want %d", p.Warnings, len(tc.warnings))
			}
			for i, w := range tc.warnings {
				if !strings.Contains(p.Warnings[i], w) {
					t.Errorf("warning %d = %q, want it to contain %q", i, p.Warnings[i], w)
				}
			}
		})
	}
}

// TestPlanExcludesBySpeed covers ADR-015: a node is not given a model whose
// predicted generation tok/s (from the node's measured bandwidth and the
// model's file size) is below the threshold. Mi 8 numbers: a bandwidth of
// 7.2 GB/s predicts 6 tok/s for a 1.2 GB model (allowed at min 3) and 3
// tok/s for a 2.4 GB model (excluded at min 4).
func TestPlanExcludesBySpeed(t *testing.T) {
	// 8 GiB of RAM so both models fit comfortably (~2.4 GiB estimate at
	// most): the only thing excluding the 4B model here is its speed.
	mi8 := node("mi8", 8, 0, "")
	mi8.GenGBps = 7.2
	small := PlanModel{ID: "qwen2.5-1.5b", SizeBytes: 1_200_000_000, CtxTrain: 2048, Layers: 2, KVHeads: 2, HeadDim: 8} // pred 6.0 tok/s
	big := PlanModel{ID: "gemma-3-4b", SizeBytes: 2_400_000_000, CtxTrain: 2048, Layers: 2, KVHeads: 2, HeadDim: 8}     // pred 3.0 tok/s
	catalog := []PlanModel{small, big}

	for _, tc := range []struct {
		name     string
		minTokS  float64
		model    string
		want     string
		warnings []string
	}{
		{name: "1.5B allowed at min 3", minTokS: 3, model: small.ID, want: "mi8=qwen2.5-1.5b/default"},
		{name: "4B excluded at min 4", minTokS: 4, model: big.ID,
			want:     "mi8=/none",
			warnings: []string{"gemma-3-4b on mi8: predicted 3.0 tok/s < 4"}},
		{name: "4B allowed at the exact boundary", minTokS: 3, model: big.ID, want: "mi8=gemma-3-4b/default"},
		{name: "no threshold never excludes", minTokS: 0, model: big.ID, want: "mi8=gemma-3-4b/default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultPlanner{MinTokS: tc.minTokS}.Plan(Spec{DefaultModel: tc.model}, []Node{mi8}, catalog)
			if got := summary(p); got != tc.want {
				t.Errorf("plan = %s, want %s", got, tc.want)
			}
			if len(p.Warnings) != len(tc.warnings) {
				t.Fatalf("warnings = %q, want %d", p.Warnings, len(tc.warnings))
			}
			for i, w := range tc.warnings {
				if !strings.Contains(p.Warnings[i], w) {
					t.Errorf("warning %d = %q, want it to contain %q", i, p.Warnings[i], w)
				}
			}
		})
	}

	// A policy's own min_tok_s overrides the planner default, for both
	// replicas/percent eligibility and pins (which still place, but warn).
	t.Run("policy min_tok_s overrides the planner default", func(t *testing.T) {
		spec := Spec{Policies: []Policy{{ModelID: big.ID, Mode: ModeReplicas, Replicas: 1, MinTokS: 2}}}
		p := DefaultPlanner{MinTokS: 4}.Plan(spec, []Node{mi8}, catalog)
		if got := summary(p); got != "mi8=gemma-3-4b/replicas" {
			t.Fatalf("plan = %s, want mi8=gemma-3-4b/replicas", got)
		}
	})
	t.Run("replicas excluded by speed warns like a missing fit", func(t *testing.T) {
		spec := Spec{Policies: []Policy{{ModelID: big.ID, Mode: ModeReplicas, Replicas: 1}}}
		p := DefaultPlanner{MinTokS: 4}.Plan(spec, []Node{mi8}, catalog)
		if got := summary(p); got != "mi8=/none" {
			t.Fatalf("plan = %s, want mi8=/none", got)
		}
		if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "none is fast enough") {
			t.Fatalf("warnings = %q", p.Warnings)
		}
	})
	t.Run("pin still places but warns when too slow", func(t *testing.T) {
		spec := Spec{Policies: []Policy{{ModelID: big.ID, Mode: ModePin, Nodes: []string{"mi8"}}}}
		p := DefaultPlanner{MinTokS: 4}.Plan(spec, []Node{mi8}, catalog)
		if got := summary(p); got != "mi8=gemma-3-4b/pin" {
			t.Fatalf("plan = %s, want mi8=gemma-3-4b/pin", got)
		}
		if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "gemma-3-4b on mi8: predicted 3.0 tok/s < 4") {
			t.Fatalf("warnings = %q", p.Warnings)
		}
	})
}

func TestPercentOf(t *testing.T) {
	for _, tc := range []struct {
		p    float64
		n    int
		want int
	}{{0, 10, 0}, {50, 0, 0}, {50, 3, 2}, {50, 5, 3}, {25, 10, 3}, {20, 10, 2}, {33, 3, 1}, {1, 3, 1}, {100, 7, 7}, {10, 5, 1}} {
		if got := percentOf(tc.p, tc.n); got != tc.want {
			t.Errorf("percentOf(%g, %d) = %d, want %d", tc.p, tc.n, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	nodes, models := []string{"a", "b"}, []string{"small", "mid", "big"}
	for _, tc := range []struct {
		name string
		spec Spec
		want string // "" = valid
	}{
		{"empty", Spec{}, ""},
		{"valid mix", Spec{Policies: []Policy{
			{ModelID: "big", Mode: ModePin, Nodes: []string{"a"}},
			{ModelID: "mid", Mode: ModeReplicas, Replicas: 2, Classes: []string{"m", "l"}},
			{ModelID: "small", Mode: ModePercent, Percent: 100, Classes: []string{"s"}},
		}, DefaultModel: "small"}, ""},
		{"unknown model", Spec{Policies: []Policy{{ModelID: "x", Mode: ModeReplicas, Replicas: 1}}}, `unknown model "x"`},
		{"unknown default", Spec{DefaultModel: "x"}, `default_model: unknown model "x"`},
		{"unknown node", Spec{Policies: []Policy{{ModelID: "big", Mode: ModePin, Nodes: []string{"z"}}}}, `pin to unknown node "z"`},
		{"pin without nodes", Spec{Policies: []Policy{{ModelID: "big", Mode: ModePin}}}, "pin needs nodes"},
		{"node pinned twice", Spec{Policies: []Policy{{ModelID: "big", Mode: ModePin, Nodes: []string{"a"}}, {ModelID: "mid", Mode: ModePin, Nodes: []string{"a"}}}}, `already pinned to "big"`},
		{"two policies for a model", Spec{Policies: []Policy{{ModelID: "big", Mode: ModeReplicas, Replicas: 1}, {ModelID: "big", Mode: ModePercent, Percent: 5}}}, "more than one policy"},
		{"bad mode", Spec{Policies: []Policy{{ModelID: "big", Mode: "all"}}}, "mode must be"},
		{"zero replicas", Spec{Policies: []Policy{{ModelID: "big", Mode: ModeReplicas}}}, "replicas must be at least 1"},
		{"percent out of range", Spec{Policies: []Policy{{ModelID: "big", Mode: ModePercent, Percent: 101}}}, "percent must be"},
		{"unknown class", Spec{Policies: []Policy{{ModelID: "big", Mode: ModeReplicas, Replicas: 1, Classes: []string{"huge"}}}}, `unknown device class "huge"`},
		{"percent over 100 on overlapping classes", Spec{Policies: []Policy{
			{ModelID: "small", Mode: ModePercent, Percent: 60},
			{ModelID: "mid", Mode: ModePercent, Percent: 50, Classes: []string{"m"}},
		}}, `add up to 110% for device class "m"`},
		{"disjoint classes may each reach 100", Spec{Policies: []Policy{
			{ModelID: "small", Mode: ModePercent, Percent: 100, Classes: []string{"s"}},
			{ModelID: "mid", Mode: ModePercent, Percent: 100, Classes: []string{"m"}},
		}}, ""},
	} {
		err := Validate(tc.spec, nodes, models)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}
