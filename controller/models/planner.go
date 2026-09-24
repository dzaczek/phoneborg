package models

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Policy modes.
const (
	ModePin      = "pin"
	ModeReplicas = "replicas"
	ModePercent  = "percent"
)

// Assignment reasons.
const (
	ReasonPin      = "pin"
	ReasonReplicas = "replicas"
	ReasonPercent  = "percent"
	ReasonDefault  = "default"
	ReasonKeep     = "keep" // no policy applies: the node keeps what it serves
	ReasonNone     = "none" // no policy applies and the node serves nothing
)

// Policy says where one model should run.
type Policy struct {
	ModelID  string   `json:"model_id"`
	Mode     string   `json:"mode"`     // pin | replicas | percent
	Nodes    []string `json:"nodes"`    // pin: node ids
	Replicas int      `json:"replicas"` // replicas: number of nodes
	Percent  float64  `json:"percent"`  // percent: share of eligible nodes, 0..100
	Classes  []string `json:"classes"`  // optional device class filter; empty = all
}

// Spec is the operator's placement: policies plus a default model for
// nodes no policy claims.
type Spec struct {
	Policies     []Policy `json:"policies"`
	DefaultModel string   `json:"default_model"`
}

// Normalized returns spec with non-nil slices, so JSON shows [] not null.
func (s Spec) Normalized() Spec {
	out := Spec{DefaultModel: s.DefaultModel, Policies: make([]Policy, len(s.Policies))}
	for i, p := range s.Policies {
		if p.Nodes == nil {
			p.Nodes = []string{}
		}
		if p.Classes == nil {
			p.Classes = []string{}
		}
		out.Policies[i] = p
	}
	return out
}

// Node is the planner's view of a node.
type Node struct {
	ID            string
	Class         string
	RAMTotalBytes uint64
	Speed         float64 // measured generation tok/s; 0 = unknown
	CurrentModel  string  // model the node serves or is switching to
	Assigned      string  // model the previous plan gave it, for stability
	Drained       bool
}

// PlanModel is the planner's view of a catalog model.
type PlanModel struct {
	ID        string
	SizeBytes int64
	CtxTrain  int
	Layers    int
	KVHeads   int
	HeadDim   int
}

// CtxSize is the context requested for m: DefaultCtxSize capped by the
// context it was trained with.
func (m PlanModel) CtxSize() int {
	if m.CtxTrain > 0 && m.CtxTrain < DefaultCtxSize {
		return m.CtxTrain
	}
	return DefaultCtxSize
}

// EstRAM is m's RAM estimate at CtxSize with an f16 KV cache.
func (m PlanModel) EstRAM() int64 {
	return EstimateRAM(m.SizeBytes, m.Layers, m.KVHeads, m.HeadDim, m.CtxSize())
}

// Assignment is one node's row in a plan.
type Assignment struct {
	NodeID      string `json:"node_id"`
	ModelID     string `json:"model_id"`
	Reason      string `json:"reason"`
	CtxSize     int    `json:"ctx_size"`
	Slots       int    `json:"slots"`
	KVType      string `json:"kv_type"`
	EstRAMBytes int64  `json:"est_ram_bytes"`
	Fits        bool   `json:"fits"`
}

// Plan maps every planned node to a model, with warnings for policies that
// could not be met.
type Plan struct {
	Assignments []Assignment `json:"plan"`
	Warnings    []string     `json:"warnings"`
}

// Planner computes a plan. models holds only the models that are ready to
// be served; policies for other models wait, with a warning.
// Implementations must be deterministic: the same inputs give the same plan.
type Planner interface {
	Plan(spec Spec, nodes []Node, models []PlanModel) Plan
}

// DefaultPlanner implements the rules of ADR-011: one model per node; pins,
// then replicas, then percentages, then the default model; otherwise a node
// keeps what it serves. Bigger models pick first and prefer faster nodes;
// a node already serving (or assigned) a model is kept on it.
type DefaultPlanner struct{}

// Validate checks spec against the known node ids and catalog model ids.
func Validate(spec Spec, nodeIDs, modelIDs []string) error {
	known := func(ids []string, id string) bool { return slices.Contains(ids, id) }
	var errs []error
	seenModel := map[string]bool{}
	pinned := map[string]string{}
	classPercent := map[string]float64{}
	for i, p := range spec.Policies {
		at := fmt.Sprintf("policies[%d] (%s)", i, p.ModelID)
		if !known(modelIDs, p.ModelID) {
			errs = append(errs, fmt.Errorf("%s: unknown model %q", at, p.ModelID))
		}
		if seenModel[p.ModelID] {
			errs = append(errs, fmt.Errorf("%s: model has more than one policy", at))
		}
		seenModel[p.ModelID] = true
		for _, c := range p.Classes {
			if !IsClass(c) {
				errs = append(errs, fmt.Errorf("%s: unknown device class %q", at, c))
			}
		}
		switch p.Mode {
		case ModePin:
			if len(p.Nodes) == 0 {
				errs = append(errs, fmt.Errorf("%s: pin needs nodes", at))
			}
			for _, n := range p.Nodes {
				switch {
				case !known(nodeIDs, n):
					errs = append(errs, fmt.Errorf("%s: pin to unknown node %q", at, n))
				case pinned[n] != "":
					errs = append(errs, fmt.Errorf("%s: node %q is already pinned to %q", at, n, pinned[n]))
				default:
					pinned[n] = p.ModelID
				}
			}
		case ModeReplicas:
			if p.Replicas < 1 {
				errs = append(errs, fmt.Errorf("%s: replicas must be at least 1", at))
			}
		case ModePercent:
			if !(p.Percent > 0 && p.Percent <= 100) {
				errs = append(errs, fmt.Errorf("%s: percent must be above 0 and at most 100", at))
			}
			for _, c := range Classes {
				if matchesClass(p.Classes, c.ID) {
					classPercent[c.ID] += p.Percent
				}
			}
		default:
			errs = append(errs, fmt.Errorf("%s: mode must be %q, %q or %q", at, ModePin, ModeReplicas, ModePercent))
		}
	}
	for _, c := range Classes {
		if classPercent[c.ID] > 100 {
			errs = append(errs, fmt.Errorf("percent policies add up to %g%% for device class %q (max 100)", classPercent[c.ID], c.ID))
		}
	}
	if spec.DefaultModel != "" && !known(modelIDs, spec.DefaultModel) {
		errs = append(errs, fmt.Errorf("default_model: unknown model %q", spec.DefaultModel))
	}
	return errors.Join(errs...)
}

func matchesClass(filter []string, class string) bool {
	return len(filter) == 0 || slices.Contains(filter, class)
}

// Plan implements Planner.
func (DefaultPlanner) Plan(spec Spec, nodes []Node, models []PlanModel) Plan {
	nodes = slices.Clone(nodes)
	slices.SortFunc(nodes, func(a, b Node) int { return cmp.Compare(a.ID, b.ID) })
	byModel := map[string]PlanModel{}
	for _, m := range models {
		byModel[m.ID] = m
	}
	byNode := map[string]Node{}
	for _, n := range nodes {
		byNode[n.ID] = n
	}
	var warnings []string
	warn := func(format string, a ...any) { warnings = append(warnings, fmt.Sprintf(format, a...)) }
	assigned := map[string]Assignment{}
	assign := func(n Node, m PlanModel, reason string) {
		est := m.EstRAM()
		assigned[n.ID] = Assignment{NodeID: n.ID, ModelID: m.ID, Reason: reason, CtxSize: m.CtxSize(), Slots: 1,
			KVType: "auto", EstRAMBytes: est, Fits: Fits(est, n.RAMTotalBytes)}
	}

	// Policies for models that are not ready yet (or were removed) wait.
	for _, p := range spec.Policies {
		if _, ok := byModel[p.ModelID]; !ok {
			warn("%s: model is not ready; its %s policy waits", p.ModelID, p.Mode)
		}
	}
	if _, ok := byModel[spec.DefaultModel]; spec.DefaultModel != "" && !ok {
		warn("default model %s is not ready; nodes keep their current model", spec.DefaultModel)
	}

	// Pins.
	for _, p := range spec.Policies {
		m, ok := byModel[p.ModelID]
		if p.Mode != ModePin || !ok {
			continue
		}
		for _, id := range p.Nodes {
			n, ok := byNode[id]
			switch {
			case !ok:
				warn("%s: pinned node %s is not connected", p.ModelID, id)
			case assigned[id].ModelID != "":
				warn("%s: node %s is already pinned to %s", p.ModelID, id, assigned[id].ModelID)
			default:
				assign(n, m, ReasonPin)
				if !assigned[id].Fits {
					warn("%s: pinned to %s (%s) but needs about %s with a %d-token context", p.ModelID, id, gib(n.RAMTotalBytes), gib(uint64(m.EstRAM())), m.CtxSize())
				}
			}
		}
	}

	// Replicas, then percentages; bigger models choose first.
	for _, mode := range []string{ModeReplicas, ModePercent} {
		var ps []Policy
		for _, p := range spec.Policies {
			if _, ok := byModel[p.ModelID]; ok && p.Mode == mode {
				ps = append(ps, p)
			}
		}
		slices.SortStableFunc(ps, func(a, b Policy) int {
			return cmp.Or(cmp.Compare(byModel[b.ModelID].EstRAM(), byModel[a.ModelID].EstRAM()), cmp.Compare(a.ModelID, b.ModelID))
		})
		for _, p := range ps {
			m := byModel[p.ModelID]
			eligible := func(n Node) bool {
				return !n.Drained && matchesClass(p.Classes, n.Class) && Fits(m.EstRAM(), n.RAMTotalBytes)
			}
			want := p.Replicas
			if mode == ModePercent {
				pool := 0
				for _, n := range nodes {
					if eligible(n) {
						pool++
					}
				}
				want = percentOf(p.Percent, pool)
			}
			var free []Node
			for _, n := range nodes {
				if eligible(n) && assigned[n.ID].ModelID == "" {
					free = append(free, n)
				}
			}
			slices.SortStableFunc(free, func(a, b Node) int { return preferFor(m.ID, a, b) })
			got := min(want, len(free))
			for _, n := range free[:got] {
				assign(n, m, mode)
			}
			if got < want {
				warn("%s: wants %d node(s) (%s), only %d eligible node(s) free (not drained, class filter, needs about %s)",
					m.ID, want, describe(p), got, gib(uint64(m.EstRAM())))
			}
		}
	}

	// Default model, else keep.
	dm, hasDefault := byModel[spec.DefaultModel]
	var tooSmall []string
	out := make([]Assignment, 0, len(nodes))
	for _, n := range nodes {
		a, ok := assigned[n.ID]
		if !ok && hasDefault && !n.Drained {
			if Fits(dm.EstRAM(), n.RAMTotalBytes) {
				assign(n, dm, ReasonDefault)
				a, ok = assigned[n.ID], true
			} else {
				tooSmall = append(tooSmall, n.ID)
			}
		}
		if !ok {
			a = Assignment{NodeID: n.ID, ModelID: n.CurrentModel, Reason: ReasonKeep, Fits: true}
			if cur, known := byModel[n.CurrentModel]; known {
				a.CtxSize, a.Slots, a.KVType, a.EstRAMBytes = cur.CtxSize(), 1, "auto", cur.EstRAM()
				a.Fits = Fits(a.EstRAMBytes, n.RAMTotalBytes)
			}
			if n.CurrentModel == "" {
				a.Reason = ReasonNone
			}
		}
		out = append(out, a)
	}
	if len(tooSmall) > 0 {
		warn("default model %s needs about %s and does not fit on %s; they keep their current model",
			dm.ID, gib(uint64(dm.EstRAM())), strings.Join(tooSmall, ", "))
	}
	if warnings == nil {
		warnings = []string{}
	}
	return Plan{Assignments: out, Warnings: warnings}
}

// percentOf is p% of n rounded half up, at least 1 when p > 0 and n > 0.
func percentOf(p float64, n int) int {
	if p <= 0 || n == 0 {
		return 0
	}
	return max(1, int(math.Floor(p*float64(n)/100+0.5+1e-9)))
}

// preferFor orders candidate nodes for model id: nodes already serving it,
// then nodes the previous plan gave it (so plans do not flap while a node
// switches), then faster, then bigger, then by id.
func preferFor(id string, a, b Node) int {
	rank := func(n Node) int {
		switch {
		case n.CurrentModel == id:
			return 0
		case n.Assigned == id:
			return 1
		}
		return 2
	}
	return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(b.Speed, a.Speed),
		cmp.Compare(b.RAMTotalBytes, a.RAMTotalBytes), cmp.Compare(a.ID, b.ID))
}

func describe(p Policy) string {
	s := fmt.Sprintf("replicas=%d", p.Replicas)
	if p.Mode == ModePercent {
		s = fmt.Sprintf("percent=%g", p.Percent)
	}
	if len(p.Classes) > 0 {
		s += " classes=" + strings.Join(p.Classes, ",")
	}
	return s
}

func gib(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/GiB) }
