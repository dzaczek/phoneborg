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
// It uses the default classes and reports the chosen class, its share p and the mass on the class
// letters per request, and fails only when no answer is usable; how well a
// model classifies is for the operator to judge from the log.
func TestRouterLive(t *testing.T) {
	url := os.Getenv("PHONEBORG_LLAMA_URL")
	if url == "" {
		t.Skip("set PHONEBORG_LLAMA_URL to a llama-server to run")
	}
	g, _ := newGW(t, AllowAll{})
	b := Backend{NodeID: "live", URL: url}
	st := g.router.Load() // the default classes
	cases := []struct{ text, class string }{
		{"hi, how are you?", "easy_chat"},
		{"What is the capital of France?", "easy_chat"},
		{"Translate 'good morning' to German.", "easy_writing"},
		{"Summarize in one sentence: the meeting moved to Friday because the room was booked.", "easy_writing"},
		{"Write a long fantasy story about a dragon who learns to paint, in five chapters.", "hard_writing"},
		{"Write a detailed technical article comparing PostgreSQL and MySQL replication.", "hard_writing"},
		{"How do I get the length of a string in JavaScript?", "easy_coding"},
		{"What does `ls -la` print?", "easy_coding"},
		{"Write a red-black tree in Rust with deletion and tests.", "hard_coding"},
		{"Debug this: my Go program deadlocks when a buffered channel is full.", "hard_coding"},
		{"Prove that there are infinitely many primes.", "hard_reasoning"},
		{"Plan a migration of our monolith to microservices, with rollback steps.", "hard_reasoning"},
	}
	right, usable := 0, 0
	for _, c := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		start := time.Now()
		probs, err := g.askClassifier(ctx, b, st, c.text)
		cancel()
		if err != nil { // in the gateway: a fallback to plain auto
			t.Logf("unusable: %v  %s", err, c.text)
			continue
		}
		i, p, mass := bestClass(probs, len(st.cfg.Classes))
		class := st.cfg.Classes[i].Name
		ok := mass >= routerMinMass && class == c.class
		if mass >= routerMinMass {
			usable++
		}
		if ok {
			right++
		}
		t.Logf("%-14s p=%.2f mass=%.2f want=%-14s ok=%-5v %5dms  %s", class, p, mass, c.class, ok, time.Since(start).Milliseconds(), c.text)
	}
	t.Logf("%d/%d usable (mass >= %.2f), %d/%d classified right", usable, len(cases), routerMinMass, right, len(cases))
	if usable == 0 {
		t.Error("no usable answer: the model never answered a class letter")
	}
}
