package gateway

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Backend is one node's inference server as seen by the gateway.
type Backend struct {
	NodeID string
	Model  string
	URL    string  // e.g. http://host.docker.internal:40123
	Speed  float64 // relative node speed; higher is faster, 0 = unknown (see controller.Backends, ADR-010)
	// Drained nodes get no new requests; requests already running on them
	// finish (they are not reaped).
	Drained bool
	// Hot nodes (last heartbeat temperature at/above -thermal-limit-c) get no
	// new sessions unless every candidate is hot (ADR-010).
	Hot bool
	// CtxSize is the node's context window in tokens; 0 = unknown (never
	// excluded). Prompts estimated to be longer are routed elsewhere.
	CtxSize int
	Alias   string // the node's alias, "" = none (ADR-014)
}

// Request carries what a Picker may use to decide. AffinityKey identifies
// the request's stable prompt prefix (see affinityKey); empty when unknown.
type Request struct {
	Model       string
	AffinityKey string
	PromptBytes int // request body size, a cheap proxy for prompt length
}

// Picker chooses a backend for a request. Candidates are already filtered by
// model, readiness and health. Implementations must be safe for concurrent use.
type Picker interface {
	Pick(req Request, candidates []Backend, inflight func(nodeID string) int) Backend
}

// LeastInflight picks the backend with the fewest in-flight requests,
// rotating among ties so idle nodes share load evenly. Hot candidates are
// avoided unless every one is hot (ADR-010).
type LeastInflight struct{ rr atomic.Uint64 }

func (p *LeastInflight) Pick(_ Request, c []Backend, inflight func(string) int) Backend {
	c = nonHot(c)
	return leastBy(c, int(p.rr.Add(1)%uint64(len(c))), func(b Backend) (int, int) { return inflight(b.NodeID), 0 })
}

// nonHot returns the candidates that are not overheating, or all of them if
// every one is: a hot phone is still better than no phone.
func nonHot(c []Backend) []Backend {
	out := make([]Backend, 0, len(c))
	for _, b := range c {
		if !b.Hot {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return c
	}
	return out
}

// anyNonHot reports whether at least one candidate is not overheating.
func anyNonHot(c []Backend) bool {
	for _, b := range c {
		if !b.Hot {
			return true
		}
	}
	return false
}

// leastBy returns the candidate with the smallest (primary, secondary) score,
// then the highest Speed, scanning from start so full ties rotate.
func leastBy(c []Backend, start int, score func(Backend) (int, int)) Backend {
	best := c[start]
	bp, bs := score(best)
	for i := 1; i < len(c); i++ {
		b := c[(start+i)%len(c)]
		p, s := score(b)
		if p < bp || (p == bp && s < bs) || (p == bp && s == bs && b.Speed > best.Speed) {
			best, bp, bs = b, p, s
		}
	}
	return best
}

// Affinity routes requests sharing a prompt prefix (one agent session, one
// system prompt) to the same node, so llama-server's prompt cache is reused:
// a warm 10k-token prefix costs milliseconds, a cold one minutes on a phone.
//
// New keys go to the node with the fewest in-flight requests, then the fewest
// large sessions pinned to it, then the fastest. A cold 10k-token prompt takes
// minutes, so it should land on the quickest idle phone, and one large session
// should not evict another's cache on a single-slot node. Small prompts (e.g.
// opencode's title request) are cheap to recompute and are not protected. A pinned node that is Spill requests
// busier than the least busy candidate is skipped and the key re-pinned. A
// pinned node that has become hot is skipped the same way, provided a cooler
// candidate exists (ADR-010); new sessions never land on a hot node unless
// every candidate is hot.
type Affinity struct {
	TTL         time.Duration // forget keys unused for this long
	MaxKeys     int
	Spill       int
	LargePrompt int // bytes; sessions at least this big are protected

	now       func() time.Time
	rr        atomic.Uint64
	mu        sync.Mutex
	keys      map[string]*pin
	decisions *prometheus.CounterVec
}

type pin struct {
	node  string
	last  time.Time
	large bool
}

func NewAffinity(reg prometheus.Registerer) *Affinity {
	a := &Affinity{
		TTL: 30 * time.Minute, MaxKeys: 4096, Spill: 2, LargePrompt: 16 << 10,
		now:  time.Now,
		keys: map[string]*pin{},
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "phoneborg_gateway_affinity_decisions_total",
			Help: "Routing decisions: hit (pinned node used), miss (new key), spill (pinned node too busy or gone), none (no key)."},
			[]string{"result"}),
	}
	reg.MustRegister(a.decisions)
	for _, r := range []string{"hit", "miss", "spill", "none"} {
		a.decisions.WithLabelValues(r)
	}
	return a
}

func (a *Affinity) Pick(req Request, c []Backend, inflight func(string) int) Backend {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	a.prune(now)

	sessions := map[string]int{} // large sessions per node
	for _, p := range a.keys {
		if p.large {
			sessions[p.node]++
		}
	}
	fresh := func() Backend {
		cands := nonHot(c)
		return leastBy(cands, int(a.rr.Add(1)%uint64(len(cands))), func(b Backend) (int, int) {
			return inflight(b.NodeID), sessions[b.NodeID]
		})
	}

	if req.AffinityKey == "" {
		a.decisions.WithLabelValues("none").Inc()
		return fresh()
	}
	result := "miss"
	if p := a.keys[req.AffinityKey]; p != nil {
		result = "spill"
		minInflight := inflight(c[0].NodeID)
		for _, b := range c[1:] {
			minInflight = min(minInflight, inflight(b.NodeID))
		}
		coolerExists := anyNonHot(c)
		for _, b := range c {
			if b.NodeID == p.node && !(b.Hot && coolerExists) && inflight(b.NodeID)-minInflight < a.Spill {
				p.last = now
				a.decisions.WithLabelValues("hit").Inc()
				return b
			}
		}
		if p.large {
			sessions[p.node]-- // being re-pinned, don't count it against its old node
		}
	}
	b := fresh()
	a.keys[req.AffinityKey] = &pin{node: b.NodeID, last: now, large: req.PromptBytes >= a.LargePrompt}
	a.decisions.WithLabelValues(result).Inc()
	return b
}

// prune drops expired keys, and the oldest ones beyond MaxKeys. Must hold mu.
func (a *Affinity) prune(now time.Time) {
	for k, p := range a.keys {
		if now.Sub(p.last) > a.TTL {
			delete(a.keys, k)
		}
	}
	for len(a.keys) > a.MaxKeys {
		var oldest string
		for k, p := range a.keys {
			if oldest == "" || p.last.Before(a.keys[oldest].last) {
				oldest = k
			}
		}
		delete(a.keys, oldest)
	}
}

// SetSpill changes the spill threshold for subsequent picks.
func (a *Affinity) SetSpill(n int) {
	a.mu.Lock()
	a.Spill = n
	a.mu.Unlock()
}

// SpillThreshold returns the current spill threshold.
func (a *Affinity) SpillThreshold() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Spill
}

// Pins returns the number of live sessions pinned to each node.
func (a *Affinity) Pins() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prune(a.now())
	out := map[string]int{}
	for _, p := range a.keys {
		out[p.node]++
	}
	return out
}

// Unpin forgets every session pinned to nodeID, so their next requests are
// routed afresh (e.g. when the node is drained). It returns how many moved.
func (a *Affinity) Unpin(nodeID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for k, p := range a.keys {
		if p.node == nodeID {
			delete(a.keys, k)
			n++
		}
	}
	return n
}
