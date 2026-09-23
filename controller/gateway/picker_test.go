package gateway

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var ab = []Backend{{NodeID: "a"}, {NodeID: "b"}}

func idle(string) int { return 0 }

func TestAffinitySticksToNode(t *testing.T) {
	p := NewAffinity(prometheus.NewRegistry())
	first := p.Pick(Request{AffinityKey: "s1"}, ab, idle)
	for i := 0; i < 10; i++ {
		if got := p.Pick(Request{AffinityKey: "s1"}, ab, idle); got.NodeID != first.NodeID {
			t.Fatalf("pick %d went to %s, pinned to %s", i, got.NodeID, first.NodeID)
		}
	}
	if hits := testutil.ToFloat64(p.decisions.WithLabelValues("hit")); hits != 10 {
		t.Fatalf("hits = %v", hits)
	}
}

func TestAffinitySpreadsDistinctSessions(t *testing.T) {
	p := NewAffinity(prometheus.NewRegistry())
	// Two large sessions must not share a single-slot node and evict each
	// other's cache.
	x := p.Pick(Request{AffinityKey: "s1", PromptBytes: 64 << 10}, ab, idle)
	y := p.Pick(Request{AffinityKey: "s2", PromptBytes: 64 << 10}, ab, idle)
	if x.NodeID == y.NodeID {
		t.Fatalf("both sessions pinned to %s", x.NodeID)
	}
}

func TestAffinityPrefersFasterNodeForNewSession(t *testing.T) {
	c := []Backend{{NodeID: "slow", Speed: 8}, {NodeID: "fast", Speed: 17}}
	for i := 0; i < 5; i++ { // regardless of rotation
		p := NewAffinity(prometheus.NewRegistry())
		p.rr.Store(uint64(i))
		if got := p.Pick(Request{AffinityKey: "s"}, c, idle); got.NodeID != "fast" {
			t.Fatalf("new session went to %s", got.NodeID)
		}
	}
}

func TestAffinitySmallRequestDoesNotPushLargeSessionToSlowNode(t *testing.T) {
	// opencode: a small title request arrives just before the 46 KB main one.
	c := []Backend{{NodeID: "slow", Speed: 8}, {NodeID: "fast", Speed: 17}}
	p := NewAffinity(prometheus.NewRegistry())
	p.Pick(Request{AffinityKey: "title", PromptBytes: 2 << 10}, c, idle)
	if got := p.Pick(Request{AffinityKey: "main", PromptBytes: 46 << 10}, c, idle); got.NodeID != "fast" {
		t.Fatalf("large session went to %s", got.NodeID)
	}
	// A second large session must not evict the first one's cache.
	if got := p.Pick(Request{AffinityKey: "main2", PromptBytes: 46 << 10}, c, idle); got.NodeID != "slow" {
		t.Fatalf("second large session went to %s", got.NodeID)
	}
}

func TestAffinitySpillsWhenPinnedNodeBusy(t *testing.T) {
	p := NewAffinity(prometheus.NewRegistry())
	pinned := p.Pick(Request{AffinityKey: "s"}, ab, idle).NodeID
	busy := func(n string) int {
		if n == pinned {
			return 2
		}
		return 0
	}
	if got := p.Pick(Request{AffinityKey: "s"}, ab, busy); got.NodeID == pinned {
		t.Fatal("did not spill off a node 2 requests busier")
	}
	// Re-pinned to the other node now; one extra request there is tolerated.
	other := p.Pick(Request{AffinityKey: "s"}, ab, idle).NodeID
	if got := p.Pick(Request{AffinityKey: "s"}, ab, func(n string) int {
		if n == other {
			return 1
		}
		return 0
	}); got.NodeID != other {
		t.Fatal("spilled although pinned node was only 1 request busier")
	}
}

func TestAffinityRepinsWhenNodeGone(t *testing.T) {
	p := NewAffinity(prometheus.NewRegistry())
	pinned := p.Pick(Request{AffinityKey: "s"}, ab, idle).NodeID
	var rest []Backend
	for _, b := range ab {
		if b.NodeID != pinned {
			rest = append(rest, b)
		}
	}
	got := p.Pick(Request{AffinityKey: "s"}, rest, idle)
	if got.NodeID == pinned {
		t.Fatal("picked a node that is not a candidate")
	}
	if again := p.Pick(Request{AffinityKey: "s"}, ab, idle); again.NodeID != got.NodeID {
		t.Fatalf("key not re-pinned: %s then %s", got.NodeID, again.NodeID)
	}
}

func TestAffinityExpiresKeys(t *testing.T) {
	p := NewAffinity(prometheus.NewRegistry())
	now := time.Unix(0, 0)
	p.now = func() time.Time { return now }
	p.Pick(Request{AffinityKey: "old"}, ab, idle)
	now = now.Add(31 * time.Minute)
	p.Pick(Request{AffinityKey: "new"}, ab, idle)
	if _, ok := p.keys["old"]; ok || len(p.keys) != 1 {
		t.Fatalf("keys = %v", p.keys)
	}
}

func TestAffinityKey(t *testing.T) {
	key := func(body string) string {
		var m requestMeta
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatal(err)
		}
		return m.affinityKey()
	}
	turn1 := key(`{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}],"tools":[{"x":1}]}`)
	turn2 := key(`{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}],"tools":[{"x":1}]}`)
	otherSys := key(`{"model":"m","messages":[{"role":"system","content":"T"},{"role":"user","content":"hi"}],"tools":[{"x":1}]}`)
	otherTools := key(`{"model":"m","messages":[{"role":"system","content":"S"}],"tools":[{"x":2}]}`)
	if turn1 == "" || turn1 != turn2 {
		t.Fatalf("turns of one session differ: %q %q", turn1, turn2)
	}
	if turn1 == otherSys || turn1 == otherTools {
		t.Fatal("different prefixes share a key")
	}
	if key(`{"model":"m"}`) != "" {
		t.Fatal("empty request should have no key")
	}
}
