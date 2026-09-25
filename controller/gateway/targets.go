package gateway

import (
	"errors"
	"strings"
	"sync/atomic"
)

// Virtual models (ADR-014). The "model" field of an inference request may
// name a served model or a routing target:
//
//	auto                 any ready node, normal routing
//	pool/<name>          the pool's eligible members, routed per pool
//	node/<alias-or-id>   exactly that node: no picker, no failover
//
// The gateway resolves "auto" and model ids itself; pools and nodes are
// resolved by a Targets implementation (the controller).

// Target kinds, also the "kind" of /v1/models entries.
const (
	KindModel = "model"
	KindAuto  = "auto"
	KindPool  = "pool"
	KindNode  = "node"
)

// Target is a resolved request "model".
type Target struct {
	// Label is the phoneborg_gateway_target_requests_total label: "auto",
	// "pool/<name>", "node/<alias-or-id>" or "model".
	Label string
	Model string          // served model to match; "" = any
	Nodes map[string]bool // allowed node ids; nil = any node
	// Models restricts the served models (pools with external nodes, which
	// serve several); nil = any.
	Models map[string]bool
	// Node is set for node targets: the request goes to this node only,
	// bypassing the picker, and is never retried on another node.
	Node string
	// Picker overrides the gateway's routing policy; nil = the policy set
	// with SetPicker.
	Picker Picker
}

// allows reports whether b may serve the target (model and node filters).
func (t Target) allows(b Backend) bool {
	switch {
	case t.Node != "":
		return b.NodeID == t.Node
	case t.Nodes != nil && !t.Nodes[b.NodeID]:
		return false
	case t.Models != nil && !t.Models[b.Model]:
		return false
	}
	return t.Model == "" || b.Model == t.Model
}

// filter returns the backends that may serve the target.
func (t Target) filter(all []Backend) []Backend {
	var out []Backend
	for _, b := range all {
		if t.allows(b) {
			out = append(out, b)
		}
	}
	return out
}

// ModelEntry is one entry of GET /v1/models (OpenAI's list, with extra fields).
type ModelEntry struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	OwnedBy     string `json:"owned_by"`
	Kind        string `json:"kind"`
	Nodes       int    `json:"nodes"`
	Description string `json:"description,omitempty"` // pools
	Model       string `json:"model,omitempty"`       // nodes: the served model
	Ready       *bool  `json:"ready,omitempty"`       // nodes
	External    bool   `json:"external,omitempty"`    // nodes: an external engine (ADR-016)
}

// Targets resolves pool and node targets. Implementations must be safe for
// concurrent use.
type Targets interface {
	// Resolve resolves "pool/<name>" and "node/<alias-or-id>"; false means
	// no such pool or node.
	Resolve(name string) (Target, bool)
	// Models lists the pool and node entries of GET /v1/models.
	Models() []ModelEntry
}

// ErrUnknownTarget is returned for a pool or node that does not exist.
var ErrUnknownTarget = errors.New("unknown pool or node")

// SetTargets installs the pool and node resolver. Without one, "pool/..."
// and "node/..." names are unknown.
func (g *Gateway) SetTargets(t Targets) { g.targets.Store(&t) }

// IsVirtual reports whether name is a pool or node target.
func IsVirtual(name string) bool {
	return strings.HasPrefix(name, KindPool+"/") || strings.HasPrefix(name, KindNode+"/")
}

// Resolve turns a request "model" into a Target.
func (g *Gateway) Resolve(name string) (Target, error) {
	switch {
	case name == KindAuto:
		return Target{Label: KindAuto}, nil
	case IsVirtual(name):
		if t := g.targets.Load(); t != nil && *t != nil {
			if tgt, ok := (*t).Resolve(name); ok {
				return tgt, nil
			}
		}
		return Target{}, ErrUnknownTarget
	}
	return Target{Label: KindModel, Model: name}, nil
}

// Spread picks the backend with the fewest in-flight requests, then the
// highest Speed, rotating among full ties. It keeps no session state and
// never touches affinity pins: pools of small agents (ADR-014) send short
// prompts that are cheap to recompute, so running them in parallel beats
// pinning them to a warm cache. Hot candidates are avoided unless every one
// is hot (ADR-010).
type Spread struct{ rr atomic.Uint64 }

func (p *Spread) Pick(_ Request, c []Backend, inflight func(string) int) Backend {
	c = nonHot(c)
	return leastBy(c, int(p.rr.Add(1)%uint64(len(c))), func(b Backend) (int, int) { return inflight(b.NodeID), 0 })
}
