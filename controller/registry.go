// Package controller holds node membership state and the HTTP API.
package controller

import (
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

var ErrUnknownNode = errors.New("unknown node")

// Registry is the in-memory source of truth for node membership. A single
// controller is assumed; consensus is deliberately out of scope for now.
type Registry struct {
	mu           sync.Mutex
	nodes        map[string]*proto.Node
	drained      map[string]bool // by node id; survives re-registration and Forget
	now          func() time.Time
	suspectAfter time.Duration
	offlineAfter time.Duration
	onTransition func(id string, from, to proto.NodeState)
	log          *slog.Logger
}

func NewRegistry(suspectAfter, offlineAfter time.Duration, log *slog.Logger) *Registry {
	return &Registry{
		nodes:        map[string]*proto.Node{},
		drained:      map[string]bool{},
		now:          time.Now,
		suspectAfter: suspectAfter,
		offlineAfter: offlineAfter,
		onTransition: func(string, proto.NodeState, proto.NodeState) {},
		log:          log,
	}
}

// setState must be called with mu held.
func (r *Registry) setState(n *proto.Node, to proto.NodeState, reason string) {
	from := n.State
	if from == to {
		return
	}
	n.State = to
	r.log.Info("node state transition", "node_id", n.ID, "from", from, "to", to, "reason", reason)
	r.onTransition(n.ID, from, to)
}

// Register adds a node or re-admits a known one. A re-registration resets the
// benchmark, since the node restarted and will benchmark again.
func (r *Registry) Register(req proto.RegisterRequest, remoteAddr string) *proto.Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	n, ok := r.nodes[req.NodeID]
	if !ok {
		n = &proto.Node{ID: req.NodeID, RegisteredAt: now}
		r.nodes[req.NodeID] = n
	}
	n.Inventory = req.Inventory
	n.RemoteAddr = remoteAddr
	n.Benchmark = nil
	n.LastSeen = now
	r.setState(n, proto.StateBenchmarking, "register")
	return n
}

func (r *Registry) ReportBenchmark(rep proto.BenchmarkReport) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[rep.NodeID]
	if !ok {
		return ErrUnknownNode
	}
	b := rep.Benchmark
	n.Benchmark = &b
	n.LastSeen = r.now()
	r.setState(n, proto.StateActive, "benchmark complete")
	return nil
}

// Heartbeat returns ErrUnknownNode when the controller has no record of the
// node (e.g. after a controller restart); the agent then re-registers.
func (r *Registry) Heartbeat(hb proto.Heartbeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[hb.NodeID]
	if !ok {
		return ErrUnknownNode
	}
	n.LastHeartbeat = &hb
	n.LastSeen = r.now()
	if n.State == proto.StateSuspect || n.State == proto.StateOffline {
		if n.Benchmark != nil {
			r.setState(n, proto.StateActive, "heartbeat resumed")
		} else {
			r.setState(n, proto.StateBenchmarking, "heartbeat resumed")
		}
	}
	return nil
}

// Sweep demotes nodes whose heartbeats stopped. Call it periodically.
func (r *Registry) Sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for _, n := range r.nodes {
		age := now.Sub(n.LastSeen)
		switch {
		case age >= r.offlineAfter:
			r.setState(n, proto.StateOffline, "no heartbeat for "+age.Round(time.Second).String())
		case age >= r.suspectAfter && n.State != proto.StateOffline:
			r.setState(n, proto.StateSuspect, "no heartbeat for "+age.Round(time.Second).String())
		}
	}
}

// Snapshot returns copies sorted by ID, safe to use without the lock.
func (r *Registry) Snapshot() []proto.Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshot()
}

// snapshot must be called with mu held.
func (r *Registry) snapshot() []proto.Node {
	out := make([]proto.Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// View returns Snapshot and the drained node ids, read under one lock.
func (r *Registry) View() ([]proto.Node, map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	nodes := r.snapshot()
	drained := make(map[string]bool, len(r.drained))
	for id := range r.drained {
		drained[id] = true
	}
	return nodes, drained
}

// SetDrained drains or undrains a node. A drained node gets no new inference
// requests. The flag is kept by node id, so it survives the node
// re-registering or being forgotten. Draining needs a known node (to catch
// typos); undraining also accepts an id that is only remembered as drained.
func (r *Registry) SetDrained(id string, drained bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, known := r.nodes[id]; !known && (drained || !r.drained[id]) {
		return ErrUnknownNode
	}
	if drained {
		r.drained[id] = true
	} else {
		delete(r.drained, id)
	}
	return nil
}

// Forget removes a node. If it is still alive, its next heartbeat gets
// ErrUnknownNode and it registers again.
func (r *Registry) Forget(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nodes[id]; !ok {
		return ErrUnknownNode
	}
	delete(r.nodes, id)
	return nil
}
