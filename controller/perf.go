package controller

import (
	"sync"

	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/proto"
)

// Per-node measured memory bandwidth (ADR-015): kept in memory here and
// persisted with the routing state (routing.json), like node aliases, so a
// controller restart does not lose it.

// nodePerf keeps each node's latest known-good measured bandwidth. A stale
// or missing self-test (RuntimeStatus.GenTPS/PromptTPS/ModelBytes still 0,
// e.g. mid model switch) leaves the previous value in place, so the
// controller keeps planning and displaying the last good measurement
// instead of losing it.
type nodePerf struct {
	mu   sync.Mutex
	perf map[string]models.Perf
}

func newNodePerf() *nodePerf { return &nodePerf{perf: map[string]models.Perf{}} }

// update recomputes id's bandwidth from a heartbeat's runtime status and
// reports whether the stored value changed.
func (p *nodePerf) update(id string, rt *proto.RuntimeStatus) bool {
	if rt == nil || rt.ModelBytes <= 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.perf[id]
	next := cur
	if rt.GenTPS > 0 {
		next.GenGBps = models.Bandwidth(rt.GenTPS, rt.ModelBytes)
	}
	if rt.PromptTPS > 0 {
		next.PromptGBps = models.Bandwidth(rt.PromptTPS, rt.ModelBytes)
	}
	if next == cur {
		return false
	}
	p.perf[id] = next
	return true
}

// get returns id's last known-good bandwidth, or a zero Perf if unmeasured.
func (p *nodePerf) get(id string) models.Perf {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.perf[id]
}

// snapshot copies every node's bandwidth, for persistence.
func (p *nodePerf) snapshot() map[string]models.Perf {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]models.Perf, len(p.perf))
	for id, v := range p.perf {
		out[id] = v
	}
	return out
}

// load replaces the stored bandwidth, e.g. with the persisted state at startup.
func (p *nodePerf) load(m map[string]models.Perf) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.perf = make(map[string]models.Perf, len(m))
	for id, v := range m {
		p.perf[id] = v
	}
}
