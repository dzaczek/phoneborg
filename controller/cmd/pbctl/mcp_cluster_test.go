package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGateway answers chat completions with reply(n) for the n-th request
// and names a node per request.
func fakeGateway(t *testing.T, reply func(n int, prompt string) string) *httptest.Server {
	var n atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		i := int(n.Add(1))
		w.Header().Set("X-PhoneBorg-Node", fmt.Sprintf("phone%d", i%3))
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
			"content": reply(i, req.Messages[len(req.Messages)-1].Content)}}}})
		w.Write(b)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestMCPClusterMap(t *testing.T) {
	gw := fakeGateway(t, func(_ int, p string) string { return "done: " + p })
	c := &client{base: gw.URL, http: &http.Client{}}
	text, err := mcpMap(c, map[string]any{"tasks": []any{"a", "b", "c"}, "model": "pool/code"})
	if err != nil || !strings.Contains(text, "### Task 1 (node") || !strings.Contains(text, "done: c") || !strings.Contains(text, "3 of 3 tasks answered on pool/code") {
		t.Fatalf("%v\n%s", err, text)
	}
	if _, err := mcpMap(c, map[string]any{}); err == nil {
		t.Error("no tasks accepted")
	}
}

func TestMCPClusterVote(t *testing.T) {
	// Four voters say 5, one says 2.
	gw := fakeGateway(t, func(n int, _ string) string {
		if n == 3 {
			return "ANSWER: 2\nREASON: the dragon changes colour."
		}
		return "**ANSWER:** 5\nREASON: consistent with the plot."
	})
	c := &client{base: gw.URL, http: &http.Client{}}
	text, err := mcpVote(c, map[string]any{"question": "Is it consistent?", "options": []any{"1", "2", "3", "4", "5"},
		"threshold": "4/5", "min_score": 4.0})
	if err != nil || !strings.Contains(text, "Votes (5 valid of 5): 5: 4, 2: 1. Majority: 5.") || !strings.Contains(text, "Threshold 4/5: PASS (4 of 5 approving)") {
		t.Fatalf("%v\n%s", err, text)
	}
	c.base = fakeGateway(t, func(n int, _ string) string {
		if n == 3 {
			return "ANSWER: 2\nREASON: no."
		}
		return "ANSWER: 5"
	}).URL
	text, _ = mcpVote(c, map[string]any{"question": "q", "options": []any{"1", "2", "3", "4", "5"}, "threshold": "5/5", "min_score": 4.0})
	if !strings.Contains(text, "Threshold 5/5: FAIL") {
		t.Errorf("strict threshold:\n%s", text)
	}
	if _, err := mcpVote(c, map[string]any{"question": "q", "options": []any{"yes"}}); err == nil {
		t.Error("one option accepted")
	}
}

func TestParseVote(t *testing.T) {
	for reply, want := range map[string]string{
		"ANSWER: yes\nREASON: fine": "yes", "answer: No.": "no", "**Answer**: **4**": "4", "I think yes": "", "Yes, clearly": "yes",
	} {
		if got, _ := parseVote(reply, []string{"yes", "no", "4"}); got != want {
			t.Errorf("%q: got %q, want %q", reply, got, want)
		}
	}
}

func TestMCPJobWait(t *testing.T) {
	old := mcpWaitEvery
	mcpWaitEvery = 20 * time.Millisecond
	t.Cleanup(func() { mcpWaitEvery = old })
	ts := newController(t)
	c := &client{base: ts.URL, token: token, http: &http.Client{Timeout: 10 * time.Second}}
	start, err := mcpJobStart(c, map[string]any{"goal": "g"})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Fields(start)[2]
	// The test controller runs no jobs: the job stays queued until the timeout.
	t0 := time.Now()
	text, err := mcpJobWait(c, map[string]any{"id": id, "timeout_s": 1.0})
	if err != nil || !strings.Contains(text, ": queued,") || time.Since(t0) < time.Second {
		t.Fatalf("%v after %v:\n%s", err, time.Since(t0), text)
	}
	// A cancelled job returns at once.
	if _, err := c.do(http.MethodPost, "/admin/jobs/"+id+"/cancel", nil); err != nil {
		t.Fatal(err)
	}
	t0 = time.Now()
	if text, _ := mcpJobWait(c, map[string]any{"id": id, "timeout_s": 30.0}); !strings.Contains(text, "cancelled") || time.Since(t0) > time.Second {
		t.Errorf("after %v:\n%s", time.Since(t0), text)
	}
}
