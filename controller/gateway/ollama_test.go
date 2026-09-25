package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func newOllamaGW(t *testing.T, backends ...Backend) (*Gateway, http.Handler) {
	g := New(AllowAll{}, &LeastInflight{}, func() []Backend { return backends },
		Config{UpstreamTimeout: 5 * time.Second, MaxAttempts: 2, Cooldown: time.Minute},
		prometheus.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	g.Register(mux)
	g.RegisterOllama(mux)
	return g, mux
}

func ollamaDo(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func TestOllamaVersion(t *testing.T) {
	_, h := newOllamaGW(t)
	w := ollamaDo(h, http.MethodGet, "/api/version", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"version":"0.0.0-phoneborg"`) {
		t.Fatalf("version: %d %s", w.Code, w.Body)
	}
}

func TestOllamaUnsupportedEndpoints(t *testing.T) {
	_, h := newOllamaGW(t)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/embed"}, {"POST", "/api/embeddings"}, {"POST", "/api/pull"},
		{"POST", "/api/push"}, {"POST", "/api/create"}, {"DELETE", "/api/delete"}, {"POST", "/api/copy"},
	} {
		w := ollamaDo(h, tc.method, tc.path, "{}")
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s: status %d %s", tc.method, tc.path, w.Code, w.Body)
		}
		var out map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: %v: %s", tc.method, tc.path, err, w.Body)
		}
		if !strings.Contains(out["error"], "not supported by PhoneBorg") {
			t.Fatalf("%s %s: error = %q", tc.method, tc.path, out["error"])
		}
		if tc.path == "/api/pull" && !strings.Contains(out["error"], "pbctl models add") {
			t.Fatalf("pull error should suggest `pbctl models add`: %q", out["error"])
		}
	}
}

func TestOllamaTagsShowPS(t *testing.T) {
	g, h := newOllamaGW(t, Backend{NodeID: "a", Model: "m", URL: "http://unused"})
	g.SetModelInfo(func(id string) (ModelInfo, bool) {
		if id != "m" {
			return ModelInfo{}, false
		}
		return ModelInfo{SizeBytes: 123, SHA256: strings.Repeat("a", 64), Arch: "qwen2", Params: "0.5B", Quant: "Q4_K_M"}, true
	})

	w := ollamaDo(h, http.MethodGet, "/api/tags", "")
	var tags struct {
		Models []ollamaTagModel `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tags); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	var foundModel, foundAuto bool
	for _, m := range tags.Models {
		switch m.Name {
		case "m":
			foundModel = true
			if m.Size != 123 || m.Digest != "sha256:"+strings.Repeat("a", 64) ||
				m.Details.Family != "qwen2" || m.Details.ParameterSize != "0.5B" || m.Details.QuantizationLevel != "Q4_K_M" {
				t.Fatalf("model m = %+v", m)
			}
		case "auto":
			foundAuto = true
			if m.Digest != placeholderDigest || m.Details.Family != "unknown" {
				t.Fatalf("auto placeholder = %+v", m)
			}
		}
	}
	if !foundModel || !foundAuto {
		t.Fatalf("tags = %+v", tags.Models)
	}

	w = ollamaDo(h, http.MethodPost, "/api/show", `{"model":"m"}`)
	var show struct {
		Details ollamaModelDetails `json:"details"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &show); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	if show.Details.Family != "qwen2" || show.Details.QuantizationLevel != "Q4_K_M" {
		t.Fatalf("show m = %+v", show)
	}

	w = ollamaDo(h, http.MethodPost, "/api/show", `{"model":"nope"}`)
	var show2 struct {
		Details ollamaModelDetails `json:"details"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &show2)
	if show2.Details.Family != "unknown" || show2.Details.ParameterSize != "unknown" {
		t.Fatalf("show of an unknown model should use placeholders: %+v", show2)
	}

	w = ollamaDo(h, http.MethodGet, "/api/ps", "")
	var ps struct {
		Models []ollamaTagModel `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ps); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	if len(ps.Models) != 1 || ps.Models[0].Name != "m" || ps.Models[0].Size != 123 {
		t.Fatalf("ps = %+v", ps.Models)
	}
}

// TestOllamaChatOpenAIBodyMapping is a golden test for options -> OpenAI
// fields, format "json" -> response_format and tools/messages passthrough.
func TestOllamaChatOpenAIBodyMapping(t *testing.T) {
	temp, topP, seed := 0.5, 0.9, 7
	topK, numPredict, numCtx := 40, 16, 8192
	req := ollamaChatRequest{
		Model:    "m",
		Messages: []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)},
		Format:   json.RawMessage(`"json"`),
		Tools:    json.RawMessage(`[{"type":"function","function":{"name":"f"}}]`),
		Options: &ollamaOptions{
			Temperature: &temp, TopP: &topP, TopK: &topK, NumPredict: &numPredict,
			Stop: []string{"\n"}, Seed: &seed, NumCtx: &numCtx,
		},
	}
	var got map[string]any
	if err := json.Unmarshal(req.openAIBody(true), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"model": "m", "stream": true,
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":           []any{map[string]any{"type": "function", "function": map[string]any{"name": "f"}}},
		"response_format": map[string]any{"type": "json_object"},
		"temperature":     0.5, "top_p": 0.9, "top_k": float64(40), "max_tokens": float64(16),
		"stop": []any{"\n"}, "seed": float64(7),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestOllamaOptionsNumCtxIgnoredAndUnlimitedNumPredict(t *testing.T) {
	ctx, unlimited := 8192, -1
	req := ollamaChatRequest{Model: "m", Options: &ollamaOptions{NumCtx: &ctx, NumPredict: &unlimited}}
	var got map[string]any
	if err := json.Unmarshal(req.openAIBody(false), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["num_ctx"]; ok {
		t.Error("num_ctx must be ignored (context size is a placement decision, ADR-011)")
	}
	if _, ok := got["max_tokens"]; ok {
		t.Error("num_predict -1 (unlimited) must not set max_tokens")
	}
}

func TestOllamaGenerateBodies(t *testing.T) {
	chat := ollamaGenerateRequest{Model: "m", System: "sys", Prompt: "hello"}
	var gotChat struct {
		Model    string              `json:"model"`
		Messages []map[string]string `json:"messages"`
	}
	if err := json.Unmarshal(chat.chatBody(false), &gotChat); err != nil {
		t.Fatal(err)
	}
	if len(gotChat.Messages) != 2 || gotChat.Messages[0]["role"] != "system" || gotChat.Messages[0]["content"] != "sys" ||
		gotChat.Messages[1]["role"] != "user" || gotChat.Messages[1]["content"] != "hello" {
		t.Fatalf("chatBody messages = %+v", gotChat.Messages)
	}

	raw := ollamaGenerateRequest{Model: "m", Prompt: "hello", Raw: true}
	var gotRaw map[string]any
	if err := json.Unmarshal(raw.completionBody(false), &gotRaw); err != nil {
		t.Fatal(err)
	}
	if gotRaw["prompt"] != "hello" {
		t.Fatalf("completionBody = %v", gotRaw)
	}
	if _, ok := gotRaw["messages"]; ok {
		t.Error("a raw generate body must not have messages")
	}

	// No system prompt: only the user message.
	noSys := ollamaGenerateRequest{Model: "m", Prompt: "hi"}
	var gotNoSys struct {
		Messages []map[string]string `json:"messages"`
	}
	_ = json.Unmarshal(noSys.chatBody(false), &gotNoSys)
	if len(gotNoSys.Messages) != 1 || gotNoSys.Messages[0]["role"] != "user" {
		t.Fatalf("messages without system = %+v", gotNoSys.Messages)
	}
}

// TestWriteOllamaChatResultMapping is a golden test for the non-stream
// response mapping from OpenAI usage/timings to Ollama's chat shape.
func TestWriteOllamaChatResultMapping(t *testing.T) {
	bw := newBufferedWriter()
	bw.status = http.StatusOK
	bw.body.WriteString(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":4},"timings":{"predicted_per_second":20,"prompt_per_second":50}}`)
	w := httptest.NewRecorder()
	writeOllamaChatResult(w, bw, "m", time.Now().Add(-time.Second))

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	if out["model"] != "m" || out["done"] != true || out["done_reason"] != "stop" {
		t.Fatalf("out = %v", out)
	}
	msg, _ := out["message"].(map[string]any)
	if msg["content"] != "hello" || msg["role"] != "assistant" {
		t.Fatalf("message = %v", msg)
	}
	if out["prompt_eval_count"].(float64) != 10 || out["eval_count"].(float64) != 4 {
		t.Fatalf("counts = %v", out)
	}
	// 4 tokens / 20 tok/s = 0.2s = 2e8 ns; 10 / 50 = 0.2s = 2e8 ns too.
	if out["eval_duration"].(float64) != 2e8 || out["prompt_eval_duration"].(float64) != 2e8 {
		t.Fatalf("durations = %v", out)
	}
	if out["total_duration"].(float64) <= 0 {
		t.Fatalf("total_duration = %v", out["total_duration"])
	}
}

// TestWriteOllamaChatResultError checks that the gateway's own error
// envelope is transcoded to Ollama's flat {"error":"..."} shape.
func TestWriteOllamaChatResultError(t *testing.T) {
	bw := newBufferedWriter()
	bw.status = http.StatusNotFound
	bw.body.WriteString(`{"error":{"message":"no pool or node \"x\"","type":"invalid_request_error","code":"model_not_found"}}`)
	w := httptest.NewRecorder()
	writeOllamaChatResult(w, bw, "x", time.Now())
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d", w.Code)
	}
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["error"] != `no pool or node "x"` {
		t.Fatalf("error = %q", out["error"])
	}
}

// TestOllamaStreamAdapterSSEToNDJSON drives the adapter directly, including a
// content chunk split across two Write calls, and checks the final "done"
// line carries timings mapped from the upstream's stats event.
func TestOllamaStreamAdapterSSEToNDJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	start := time.Now().Add(-500 * time.Millisecond)
	a := newOllamaStreamAdapter(rec, "m", start, ollamaFieldMessage, false)
	a.WriteHeader(http.StatusOK)
	for _, chunk := range []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"He",
		"llo\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\" there\"},\"finish_reason\":null}]}\n\n",
		"data: {\"choices\":[],\"timings\":{\"prompt_n\":5,\"prompt_per_second\":100,\"predicted_n\":2,\"predicted_per_second\":10}}\n\n",
		"data: [DONE]\n\n",
	} {
		if _, err := a.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	a.finish()

	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content-type = %q", ct)
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 NDJSON lines, got %d: %q", len(lines), rec.Body.String())
	}
	var l0, l1, l2 map[string]any
	for i, l := range []*map[string]any{&l0, &l1, &l2} {
		if err := json.Unmarshal([]byte(lines[i]), l); err != nil {
			t.Fatalf("line %d: %v: %s", i, err, lines[i])
		}
	}
	if l0["done"] != false || l0["message"].(map[string]any)["content"] != "Hello" {
		t.Fatalf("line0 = %v", l0)
	}
	if l1["done"] != false || l1["message"].(map[string]any)["content"] != " there" {
		t.Fatalf("line1 = %v", l1)
	}
	if l2["done"] != true || l2["done_reason"] != "stop" {
		t.Fatalf("line2 = %v", l2)
	}
	if l2["prompt_eval_count"].(float64) != 5 || l2["eval_count"].(float64) != 2 {
		t.Fatalf("line2 counts = %v", l2)
	}
	// 2 tokens / 10 tok/s = 0.2s = 2e8 ns.
	if l2["eval_duration"].(float64) != 2e8 {
		t.Fatalf("line2 eval_duration = %v", l2["eval_duration"])
	}
}

func TestOllamaStreamAdapterErrorPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	a := newOllamaStreamAdapter(rec, "m", time.Now(), ollamaFieldMessage, false)
	a.WriteHeader(http.StatusServiceUnavailable)
	_, _ = a.Write([]byte(`{"error":{"message":"no ready nodes","type":"invalid_request_error"}}`))
	a.finish()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", rec.Code)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["error"] != "no ready nodes" {
		t.Fatalf("error = %q", out["error"])
	}
}

// TestOllamaChatEndToEnd drives the real gateway (routing, affinity, usage,
// metrics) with a fake llama-server backend, for both response modes.
func TestOllamaChatEndToEnd(t *testing.T) {
	a := fakeLlama(t, "a")
	_, h := newOllamaGW(t, Backend{NodeID: "a", Model: "m", URL: a.URL})

	w := ollamaDo(h, http.MethodPost, "/api/chat", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("non-stream: status %d %s", w.Code, w.Body)
	}
	var out struct {
		Model   string                   `json:"model"`
		Message struct{ Content string } `json:"message"`
		Done    bool                     `json:"done"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	if out.Model != "m" || out.Message.Content != "hi from a" || !out.Done {
		t.Fatalf("non-stream result = %+v", out)
	}

	w = ollamaDo(h, http.MethodPost, "/api/chat", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`) // stream defaults true
	if w.Code != http.StatusOK {
		t.Fatalf("stream: status %d %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("stream content-type = %q", ct)
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 NDJSON lines, got %d: %q", len(lines), w.Body.String())
	}
	var first, last map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &first)
	_ = json.Unmarshal([]byte(lines[1]), &last)
	if first["done"] != false || first["message"].(map[string]any)["content"] != "hi from a" {
		t.Fatalf("first line = %v", first)
	}
	if last["done"] != true || last["eval_count"].(float64) != 7 {
		t.Fatalf("last line = %v", last)
	}
}

func TestOllamaGenerateEndToEnd(t *testing.T) {
	a := fakeLlama(t, "a")
	_, h := newOllamaGW(t, Backend{NodeID: "a", Model: "m", URL: a.URL})

	w := ollamaDo(h, http.MethodPost, "/api/generate", `{"model":"m","prompt":"hi","stream":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	var out struct {
		Response string `json:"response"`
		Done     bool   `json:"done"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	if out.Response != "hi from a" || !out.Done {
		t.Fatalf("result = %+v", out)
	}
}

func TestOllamaModelNotFound(t *testing.T) {
	_, h := newOllamaGW(t, Backend{NodeID: "a", Model: "m", URL: "http://unused"})
	w := ollamaDo(h, http.MethodPost, "/api/chat", `{"model":"nope","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["error"] == "" {
		t.Fatalf("want a flat Ollama error, got %s", w.Body)
	}
}

// TestOllamaAccessDenied checks that ADR-017's access control also gates the
// metadata-only Ollama endpoints (not just chat/generate), in Ollama's error
// shape.
func TestOllamaAccessDenied(t *testing.T) {
	keys := NewStaticKeys("")
	ac := AccessControl{Keys: keys, Mode: AccessLocal, Trusted: loopback}
	g := New(ac, &LeastInflight{}, func() []Backend { return nil },
		Config{UpstreamTimeout: time.Second, MaxAttempts: 1, Cooldown: time.Minute},
		prometheus.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	g.Register(mux)
	g.RegisterOllama(mux)

	r := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["error"] == "" {
		t.Fatalf("want a flat {\"error\":...} body, got %s", w.Body)
	}
}
