package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mcpSession sends lines to serveMCP and returns the responses by id.
func mcpSession(t *testing.T, c *client, lines ...string) map[string]map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := serveMCP(c, strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	res := map[string]map[string]any{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not one JSON message per line: %q", l)
		}
		res[fmt.Sprint(m["id"])] = m
	}
	return res
}

func toolText(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	r, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	content := r["content"].([]any)[0].(map[string]any)
	isErr, _ := r["isError"].(bool)
	return content["text"].(string), isErr
}

func TestMCPServer(t *testing.T) {
	ts := newController(t)
	c := &client{base: ts.URL, token: token, http: &http.Client{Timeout: 10 * time.Second}}
	res := mcpSession(t, c,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"job_start","arguments":{"goal":"Write a story","title":"Story"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"jobs_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"job_status","arguments":{"id":"nope"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"job_start","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"cluster_status","arguments":{}}}`,
		`not json`,
	)
	if len(res) != 11 { // ids 1-10 and the parse error; the notification gets no answer
		t.Fatalf("responses %v", res)
	}
	init := res["1"]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" || init["capabilities"].(map[string]any)["tools"] == nil {
		t.Errorf("initialize %v", init)
	}
	if tools := res["2"]["result"].(map[string]any)["tools"].([]any); len(tools) != 10 {
		t.Errorf("%d tools", len(tools))
	}
	if text, isErr := toolText(t, res["4"]); isErr || !strings.Contains(text, `Started job`) || !strings.Contains(text, `"Story"`) {
		t.Errorf("job_start: %q", text)
	}
	if text, _ := toolText(t, res["5"]); !strings.Contains(text, `"Story": queued`) {
		t.Errorf("jobs_list: %q", text)
	}
	if text, isErr := toolText(t, res["6"]); !isErr || !strings.Contains(text, "404") {
		t.Errorf("job_status of an unknown job: %q", text)
	}
	if text, isErr := toolText(t, res["7"]); !isErr || text != "goal is required" {
		t.Errorf("job_start without goal: %q", text)
	}
	if res["8"]["error"].(map[string]any)["code"].(float64) != -32602 || res["9"]["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Errorf("errors %v %v", res["8"], res["9"])
	}
	if text, _ := toolText(t, res["10"]); !strings.Contains(text, "- n1: BENCHMARKING") {
		t.Errorf("cluster_status: %q", text)
	}
	if res["<nil>"]["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Errorf("parse error %v", res["<nil>"])
	}
}

func TestMCPAskCluster(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"<think>x</think>\n%s %d"}}]}`, req.Model, req.MaxTokens)
	}))
	defer gw.Close()
	c := &client{base: gw.URL, http: &http.Client{}}
	res := mcpSession(t, c, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask_cluster","arguments":{"prompt":"hi","model":"pool/smart","max_tokens":64}}}`)
	if text, isErr := toolText(t, res["1"]); isErr || text != "pool/smart 64" {
		t.Errorf("ask_cluster: %q", text)
	}
}
