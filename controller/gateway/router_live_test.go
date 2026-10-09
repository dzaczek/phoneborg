package gateway

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestRouterLive runs the classifier against a real llama-server, e.g. a
// phone's (adb forward) or one on the host:
//
//	PHONEBORG_LLAMA_URL=http://127.0.0.1:18431 go test ./controller/gateway -run TestRouterLive -v
//
// It reports P(hard) and the A+B mass per request, and fails only when no
// answer is usable; how well a model classifies is for the operator to
// judge from the log.
func TestRouterLive(t *testing.T) {
	url := os.Getenv("PHONEBORG_LLAMA_URL")
	if url == "" {
		t.Skip("set PHONEBORG_LLAMA_URL to a llama-server to run")
	}
	g, _ := newGW(t, AllowAll{})
	b := Backend{NodeID: "live", URL: url}
	cases := []struct {
		text string
		hard bool
	}{
		{"hi, how are you?", false},
		{"What is the capital of France?", false},
		{"Translate 'good morning' to German.", false},
		{"Write a red-black tree in Rust with deletion.", true},
		{"Prove that there are infinitely many primes.", true},
		{"Plan a migration of our monolith to Kubernetes, with rollback steps.", true},
		{"Debug this: my Go program deadlocks when a buffered channel is full.", true},
	}
	right, usable := 0, 0
	for _, c := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		start := time.Now()
		pHard, mass, err := g.askClassifier(ctx, b, c.text)
		cancel()
		if err != nil { // in the gateway: a fallback to plain auto
			t.Logf("unusable: %v  %s", err, c.text)
			continue
		}
		ok := mass >= routerMinMass && (pHard >= DefaultRouterConfig.Threshold) == c.hard
		if mass >= routerMinMass {
			usable++
		}
		if ok {
			right++
		}
		t.Logf("p_hard=%.3f mass=%.3f want_hard=%v ok=%v %4dms  %s", pHard, mass, c.hard, ok, time.Since(start).Milliseconds(), c.text)
	}
	t.Logf("%d/%d usable (mass >= %.2f), %d/%d classified right", usable, len(cases), routerMinMass, right, len(cases))
	if usable == 0 {
		t.Error("no usable answer: the model never answered A or B")
	}
}
