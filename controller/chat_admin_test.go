package controller

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/proto"
)

// Admin API for the panel's "Chat test" feature: POST /admin/chat/completions
// and GET /admin/chat/models (ADR-018).

func TestAdminChatCompletionsRequiresToken(t *testing.T) {
	e := newEnv(t, testToken, nil, "a")
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	for _, tok := range []string{"", "wrong", testToken + "x"} {
		if w := e.do(http.MethodPost, "/admin/chat/completions", body, tok); w.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: %d %s", tok, w.Code, w.Body)
		}
	}
}

func TestAdminChatDisabledWithoutAdminToken(t *testing.T) {
	e := newEnv(t, "", nil, "a")
	if w := e.do(http.MethodPost, "/admin/chat/completions", `{}`, "whatever"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("completions: %d", w.Code)
	}
	if w := e.do(http.MethodGet, "/admin/chat/models", "", "whatever"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("models: %d", w.Code)
	}
}

func TestAdminChatCompletionsProxiesAndAttributesUsage(t *testing.T) {
	e := newEnv(t, testToken, nil, "a")
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	var res struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	e.admin(http.MethodPost, "/admin/chat/completions", body, 200, &res)
	if len(res.Choices) != 1 || res.Choices[0].Message.Content != "hi" {
		t.Fatalf("response = %+v", res)
	}

	w := e.do(http.MethodPost, "/admin/chat/completions", body, testToken)
	if node := w.Header().Get("X-PhoneBorg-Node"); node != "a" {
		t.Fatalf("X-PhoneBorg-Node = %q", node)
	}

	// Usage is attributed to the panel's own identity, not anonymous/local,
	// and to the node that actually served it, same as a real client request.
	var st Stats
	e.admin(http.MethodGet, "/admin/stats", "", 200, &st)
	k, n := st.SinceStart.Keys[PanelPrincipal], st.SinceStart.Nodes["a"]
	if k.Requests != 2 || k.CompletionTokens != 16 {
		t.Fatalf("panel usage = %+v", k)
	}
	if n.Requests != 2 {
		t.Fatalf("node usage = %+v", n)
	}
	if _, ok := st.SinceStart.Keys[gateway.Anonymous]; ok {
		t.Fatal("chat-test traffic attributed to anonymous instead of panel")
	}
}

func TestAdminChatCompletionsUnknownModel(t *testing.T) {
	e := newEnv(t, testToken, nil, "a")
	body := `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`
	if w := e.do(http.MethodPost, "/admin/chat/completions", body, testToken); w.Code != http.StatusNotFound {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
}

func TestAdminChatModels(t *testing.T) {
	e := newEnv(t, testToken, nil, "a")
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"data"`
	}
	e.admin(http.MethodGet, "/admin/chat/models", "", 200, &out)
	var ids []string
	for _, m := range out.Data {
		ids = append(ids, m.ID+":"+m.Kind)
	}
	if !slices.Contains(ids, "m:model") || !slices.Contains(ids, "auto:auto") {
		t.Fatalf("models = %+v", out.Data)
	}
}

// fakeSSELlama mimics llama-server's streaming chat completion response
// (like controller/gateway's own test fakes), so streaming end to end
// through POST /admin/chat/completions can be exercised.
func fakeSSELlama(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		fmt.Fprint(w, "data: {\"choices\":[],\"timings\":{\"predicted_n\":3,\"predicted_per_second\":9}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(s.Close)
	return s
}

func TestAdminChatCompletionsStreaming(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := NewRegistry(time.Hour, time.Hour, log)
	srv := NewServer(reg, time.Second, GatewayOptions{
		Config: gateway.Config{UpstreamTimeout: 5 * time.Second, MaxAttempts: 1, Cooldown: time.Minute},
	}, AdminOptions{Token: testToken}, log)

	backend := fakeSSELlama(t)
	u, _ := url.Parse(backend.URL)
	port, _ := strconv.Atoi(u.Port())
	reg.Register(proto.RegisterRequest{NodeID: "a"}, "x")
	_ = reg.ReportBenchmark(proto.BenchmarkReport{NodeID: "a"})
	_ = reg.Heartbeat(proto.Heartbeat{NodeID: "a",
		Runtime: &proto.RuntimeStatus{Model: "m", Ready: true, AdvertiseHost: u.Hostname(), AdvertisePort: port}})

	r := httptest.NewRequest(http.MethodPost, "/admin/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	r.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") || !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("stream not passed through: %s", rec.Body)
	}
	if !rec.Flushed {
		t.Fatal("response was not flushed while streaming (admin wrapper must unwrap to the real ResponseWriter)")
	}
}
