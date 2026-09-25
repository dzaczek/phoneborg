// Ollama-compatible front end (ADR-017). Many clients (Open WebUI, Continue,
// Raycast, ...) speak Ollama's API rather than OpenAI's. These handlers
// translate to the gateway's existing /v1/chat/completions and
// /v1/completions handling by building an internal *http.Request and calling
// g.handleProxy directly, so routing, targets (auto/pool/node), auth,
// failover, affinity, usage and metrics all apply exactly as they do for
// /v1/... clients: no routing logic is duplicated here. GET/POST endpoints
// that only describe the cluster (tags, show, ps, version) read the same
// state the OpenAI side exposes; the unsupported endpoints (pull, push,
// create, delete, copy, embed) answer 501 in Ollama's error shape.
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// ModelInfo is optional catalog metadata for a model id, used to fill in the
// Ollama-compatible /api/tags and /api/show responses. The gateway keeps no
// catalog of its own; the controller supplies this with SetModelInfo.
type ModelInfo struct {
	SizeBytes int64
	SHA256    string
	Arch      string // Ollama's "family", e.g. "qwen2"
	Params    string // e.g. "0.5B"
	Quant     string // e.g. "Q4_K_M"
}

// ModelInfoFunc looks up a model's catalog metadata; ok is false when
// unknown (a virtual model, or a served model outside the catalog).
type ModelInfoFunc func(modelID string) (ModelInfo, bool)

// SetModelInfo installs the catalog metadata lookup for Ollama responses.
// Without one (or for an id it does not know), /api/tags, /api/show and
// /api/ps fall back to placeholders.
func (g *Gateway) SetModelInfo(f ModelInfoFunc) { g.modelInfo.Store(&f) }

func (g *Gateway) modelInfoFor(id string) (ModelInfo, bool) {
	p := g.modelInfo.Load()
	if p == nil || *p == nil {
		return ModelInfo{}, false
	}
	return (*p)(id)
}

// RegisterOllama adds the Ollama-compatible routes to mux, on the main
// listener and, optionally, a second one (-ollama-listen).
func (g *Gateway) RegisterOllama(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/version", g.handleOllamaVersion)
	mux.HandleFunc("GET /api/tags", g.handleOllamaTags)
	mux.HandleFunc("POST /api/show", g.handleOllamaShow)
	mux.HandleFunc("GET /api/ps", g.handleOllamaPS)
	mux.HandleFunc("POST /api/chat", g.handleOllamaChat)
	mux.HandleFunc("POST /api/generate", g.handleOllamaGenerate)
	for _, p := range []struct{ method, path string }{
		{"POST", "/api/embed"}, {"POST", "/api/embeddings"}, {"POST", "/api/pull"},
		{"POST", "/api/push"}, {"POST", "/api/create"}, {"DELETE", "/api/delete"}, {"POST", "/api/copy"},
	} {
		mux.HandleFunc(p.method+" "+p.path, g.handleOllamaUnsupported(p.path))
	}
}

// ollamaError writes an Ollama-style error body: {"error": "<message>"}.
func ollamaError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeOllamaJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ollamaAuth authenticates endpoints that are not proxied through
// handleProxy (which authenticates the internal request itself). On failure
// it writes the Ollama-style error (403 for ADR-017's "local" mode, else
// 401) and returns ok=false.
func (g *Gateway) ollamaAuth(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, err := g.auth.Authenticate(r)
	if err != nil {
		status, reason := http.StatusUnauthorized, "unauthorized"
		if errors.Is(err, ErrRemoteRequiresKey) {
			status, reason = http.StatusForbidden, "remote_requires_api_key"
		}
		g.mRejected.WithLabelValues(reason).Inc()
		ollamaError(w, status, err.Error())
		return Principal{}, false
	}
	return p, true
}

func (g *Gateway) handleOllamaVersion(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.ollamaAuth(w, r); !ok {
		return
	}
	writeOllamaJSON(w, http.StatusOK, map[string]string{"version": "0.0.0-phoneborg"})
}

func (g *Gateway) handleOllamaUnsupported(path string) http.HandlerFunc {
	msg := path + " is not supported by PhoneBorg"
	if path == "/api/pull" {
		msg += "; add models with `pbctl models add <source>` instead"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := g.ollamaAuth(w, r); !ok {
			return
		}
		ollamaError(w, http.StatusNotImplemented, msg)
	}
}

// placeholderDigest stands in for an unknown SHA-256, keeping the
// "sha256:<hex>" shape strict Ollama clients expect.
const placeholderDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

type ollamaModelDetails struct {
	ParentModel       string   `json:"parent_model"`
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families,omitempty"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
}

type ollamaTagModel struct {
	Name       string             `json:"name"`
	Model      string             `json:"model"`
	ModifiedAt time.Time          `json:"modified_at"`
	Size       int64              `json:"size"`
	Digest     string             `json:"digest"`
	Details    ollamaModelDetails `json:"details"`
}

// ollamaModelFor builds a tags/show/ps entry named id, filled from the
// catalog when known (looked up by servedModel when given, e.g. for a
// node/<alias> target, else by id itself); placeholders otherwise.
func (g *Gateway) ollamaModelFor(id, servedModel string) ollamaTagModel {
	lookup := id
	if servedModel != "" {
		lookup = servedModel
	}
	m := ollamaTagModel{
		Name: id, Model: id, ModifiedAt: time.Now().UTC(), Digest: placeholderDigest,
		Details: ollamaModelDetails{Format: "gguf", Family: "unknown", ParameterSize: "unknown", QuantizationLevel: "unknown"},
	}
	info, ok := g.modelInfoFor(lookup)
	if !ok {
		return m
	}
	m.Size = info.SizeBytes
	if info.SHA256 != "" {
		m.Digest = "sha256:" + info.SHA256
	}
	if info.Arch != "" {
		m.Details.Family, m.Details.Families = info.Arch, []string{info.Arch}
	}
	if info.Params != "" {
		m.Details.ParameterSize = info.Params
	}
	if info.Quant != "" {
		m.Details.QuantizationLevel = info.Quant
	}
	return m
}

// handleOllamaTags is GET /api/tags: the same models GET /v1/models lists
// (served models, then auto/pool/node targets), in Ollama's list format.
func (g *Gateway) handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.ollamaAuth(w, r); !ok {
		return
	}
	entries := g.modelEntries()
	out := make([]ollamaTagModel, 0, len(entries))
	for _, e := range entries {
		out = append(out, g.ollamaModelFor(e.ID, e.Model))
	}
	writeOllamaJSON(w, http.StatusOK, map[string]any{"models": out})
}

// handleOllamaShow is POST /api/show: minimal per-model info.
func (g *Gateway) handleOllamaShow(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.ollamaAuth(w, r); !ok {
		return
	}
	var req struct {
		Model string `json:"model"`
		Name  string `json:"name"` // older Ollama clients
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		ollamaError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	id := req.Model
	if id == "" {
		id = req.Name
	}
	if id == "" {
		ollamaError(w, http.StatusBadRequest, "model is required")
		return
	}
	m := g.ollamaModelFor(id, "")
	writeOllamaJSON(w, http.StatusOK, struct {
		Modelfile  string             `json:"modelfile"`
		Details    ollamaModelDetails `json:"details"`
		ModifiedAt time.Time          `json:"modified_at"`
	}{Modelfile: "# PhoneBorg: no Modelfile is kept; served over the cluster's OpenAI-compatible gateway", Details: m.Details, ModifiedAt: m.ModifiedAt})
}

// handleOllamaPS is GET /api/ps: models currently loaded on ready nodes.
func (g *Gateway) handleOllamaPS(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.ollamaAuth(w, r); !ok {
		return
	}
	seen := map[string]bool{}
	out := []ollamaTagModel{}
	for _, b := range g.routable() {
		if b.Model == "" || seen[b.Model] {
			continue
		}
		seen[b.Model] = true
		out = append(out, g.ollamaModelFor(b.Model, ""))
	}
	writeOllamaJSON(w, http.StatusOK, map[string]any{"models": out})
}

// ollamaOptions maps Ollama's generation options to OpenAI/llama-server
// fields; NumCtx is ignored (context size is a per-node placement decision,
// ADR-011).
type ollamaOptions struct {
	Temperature *float64 `json:"temperature"`
	TopP        *float64 `json:"top_p"`
	TopK        *int     `json:"top_k"`
	NumPredict  *int     `json:"num_predict"`
	Stop        []string `json:"stop"`
	Seed        *int     `json:"seed"`
	NumCtx      *int     `json:"num_ctx"` // ignored
}

func (o *ollamaOptions) applyTo(m map[string]any) {
	if o == nil {
		return
	}
	if o.Temperature != nil {
		m["temperature"] = *o.Temperature
	}
	if o.TopP != nil {
		m["top_p"] = *o.TopP
	}
	if o.TopK != nil {
		m["top_k"] = *o.TopK
	}
	if o.NumPredict != nil && *o.NumPredict >= 0 {
		m["max_tokens"] = *o.NumPredict
	}
	if len(o.Stop) > 0 {
		m["stop"] = o.Stop
	}
	if o.Seed != nil {
		m["seed"] = *o.Seed
	}
}

// isJSONFormat reports whether an Ollama "format" field is the string
// "json" (the only value this front end maps, to response_format
// json_object); a schema object is left untranslated.
func isJSONFormat(f json.RawMessage) bool {
	var s string
	return len(f) > 0 && json.Unmarshal(f, &s) == nil && s == "json"
}

func addResponseFormat(m map[string]any, format json.RawMessage) {
	if isJSONFormat(format) {
		m["response_format"] = map[string]string{"type": "json_object"}
	}
}

// ollamaChatRequest is the body of POST /api/chat.
type ollamaChatRequest struct {
	Model    string            `json:"model"`
	Messages []json.RawMessage `json:"messages"`
	Stream   *bool             `json:"stream"`
	Format   json.RawMessage   `json:"format"`
	Options  *ollamaOptions    `json:"options"`
	Tools    json.RawMessage   `json:"tools"`
}

// openAIBody builds the /v1/chat/completions request body. Ollama's message
// and tool schemas already use OpenAI's field names (role/content,
// type+function), so both are forwarded unchanged.
func (req ollamaChatRequest) openAIBody(stream bool) []byte {
	messages := req.Messages
	if messages == nil {
		messages = []json.RawMessage{}
	}
	m := map[string]any{"model": req.Model, "messages": messages, "stream": stream}
	if len(req.Tools) > 0 && string(req.Tools) != "null" {
		m["tools"] = req.Tools
	}
	addResponseFormat(m, req.Format)
	req.Options.applyTo(m)
	b, _ := json.Marshal(m)
	return b
}

// ollamaGenerateRequest is the body of POST /api/generate.
type ollamaGenerateRequest struct {
	Model   string          `json:"model"`
	Prompt  string          `json:"prompt"`
	System  string          `json:"system"`
	Stream  *bool           `json:"stream"`
	Format  json.RawMessage `json:"format"`
	Options *ollamaOptions  `json:"options"`
	Raw     bool            `json:"raw"`
}

// chatBody builds a /v1/chat/completions request from system+prompt (raw=false).
func (req ollamaGenerateRequest) chatBody(stream bool) []byte {
	var messages []map[string]string
	if req.System != "" {
		messages = append(messages, map[string]string{"role": "system", "content": req.System})
	}
	messages = append(messages, map[string]string{"role": "user", "content": req.Prompt})
	m := map[string]any{"model": req.Model, "messages": messages, "stream": stream}
	addResponseFormat(m, req.Format)
	req.Options.applyTo(m)
	b, _ := json.Marshal(m)
	return b
}

// completionBody builds a /v1/completions request from the raw prompt (raw=true).
func (req ollamaGenerateRequest) completionBody(stream bool) []byte {
	m := map[string]any{"model": req.Model, "prompt": req.Prompt, "stream": stream}
	addResponseFormat(m, req.Format)
	req.Options.applyTo(m)
	b, _ := json.Marshal(m)
	return b
}

// internalRequest builds the *http.Request handleProxy is called with,
// carrying over the caller's auth headers (so g.auth.Authenticate sees the
// same principal) and request id.
func (g *Gateway) internalRequest(r *http.Request, path string, body []byte) *http.Request {
	ir, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, path, bytes.NewReader(body))
	ir.Header.Set("Content-Type", "application/json")
	if v := r.Header.Get("Authorization"); v != "" {
		ir.Header.Set("Authorization", v)
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		ir.Header.Set("x-api-key", v)
	}
	if v := r.Header.Get("X-Request-Id"); v != "" {
		ir.Header.Set("X-Request-Id", v)
	}
	ir.RemoteAddr = r.RemoteAddr
	return ir
}

// ollamaStats is the timing/count block shared by chat and generate
// responses, mapped from OpenAI usage and llama.cpp timings.
type ollamaStats struct {
	TotalDuration      int64 `json:"total_duration"`
	LoadDuration       int64 `json:"load_duration"`
	PromptEvalCount    int   `json:"prompt_eval_count"`
	PromptEvalDuration int64 `json:"prompt_eval_duration"`
	EvalCount          int   `json:"eval_count"`
	EvalDuration       int64 `json:"eval_duration"`
}

func fillStats(s *ollamaStats, u usage, start time.Time) {
	s.TotalDuration = time.Since(start).Nanoseconds()
	s.PromptEvalCount = u.PromptTokens
	s.EvalCount = u.CompletionTokens
	if u.PromptTPS > 0 && u.PromptTokens > 0 {
		s.PromptEvalDuration = int64(float64(u.PromptTokens) / u.PromptTPS * float64(time.Second))
	}
	if u.GenTPS > 0 && u.CompletionTokens > 0 {
		s.EvalDuration = int64(float64(u.CompletionTokens) / u.GenTPS * float64(time.Second))
	}
}

// extractOpenAIErrorMessage reads the "message" the gateway's own error
// envelope ({"error":{"message":...}}) carries, so it can be re-shown in
// Ollama's flat {"error":"..."} form; a body that is already flat, or
// unrecognised, is used as-is.
func extractOpenAIErrorMessage(body []byte) string {
	var nested struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &nested) == nil && nested.Error.Message != "" {
		return nested.Error.Message
	}
	var flat struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &flat) == nil && flat.Error != "" {
		return flat.Error
	}
	return strings.TrimSpace(string(body))
}

// bufferedWriter captures a whole response for translation: used for
// non-streaming Ollama replies and to catch an error the gateway wrote
// before reaching a backend (auth, model resolution, context size, ...).
type bufferedWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferedWriter() *bufferedWriter {
	return &bufferedWriter{header: http.Header{}, status: http.StatusOK}
}

func (b *bufferedWriter) Header() http.Header         { return b.header }
func (b *bufferedWriter) WriteHeader(code int)        { b.status = code }
func (b *bufferedWriter) Write(p []byte) (int, error) { return b.body.Write(p) }

func (g *Gateway) handleOllamaChat(w http.ResponseWriter, r *http.Request) {
	var req ollamaChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		ollamaError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	stream := true // Ollama defaults to streaming
	if req.Stream != nil {
		stream = *req.Stream
	}
	ir := g.internalRequest(r, "/v1/chat/completions", req.openAIBody(stream))
	start := time.Now()
	if stream {
		a := newOllamaStreamAdapter(w, req.Model, start, ollamaFieldMessage, false)
		g.handleProxy(a, ir)
		a.finish()
		return
	}
	bw := newBufferedWriter()
	g.handleProxy(bw, ir)
	writeOllamaChatResult(w, bw, req.Model, start)
}

func (g *Gateway) handleOllamaGenerate(w http.ResponseWriter, r *http.Request) {
	var req ollamaGenerateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		ollamaError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	stream := true
	if req.Stream != nil {
		stream = *req.Stream
	}
	path, body := "/v1/chat/completions", req.chatBody(stream)
	if req.Raw {
		path, body = "/v1/completions", req.completionBody(stream)
	}
	ir := g.internalRequest(r, path, body)
	start := time.Now()
	if stream {
		a := newOllamaStreamAdapter(w, req.Model, start, ollamaFieldResponse, req.Raw)
		g.handleProxy(a, ir)
		a.finish()
		return
	}
	bw := newBufferedWriter()
	g.handleProxy(bw, ir)
	writeOllamaGenerateResult(w, bw, req.Model, start)
}

func writeOllamaChatResult(w http.ResponseWriter, bw *bufferedWriter, model string, start time.Time) {
	if bw.status >= 400 {
		ollamaError(w, bw.status, extractOpenAIErrorMessage(bw.body.Bytes()))
		return
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Role      string          `json:"role"`
				Content   string          `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(bw.body.Bytes(), &resp)
	u, _ := parseUsage(bw.body.Bytes(), false)
	out := struct {
		Model     string    `json:"model"`
		CreatedAt time.Time `json:"created_at"`
		Message   struct {
			Role      string          `json:"role"`
			Content   string          `json:"content"`
			ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
		} `json:"message"`
		Done       bool   `json:"done"`
		DoneReason string `json:"done_reason,omitempty"`
		ollamaStats
	}{Model: model, CreatedAt: time.Now().UTC(), Done: true, DoneReason: "stop"}
	out.Message.Role = "assistant"
	if len(resp.Choices) > 0 {
		c := resp.Choices[0]
		if c.Message.Role != "" {
			out.Message.Role = c.Message.Role
		}
		out.Message.Content = c.Message.Content
		if len(c.Message.ToolCalls) > 0 && string(c.Message.ToolCalls) != "null" {
			out.Message.ToolCalls = c.Message.ToolCalls
		}
		if c.FinishReason != "" {
			out.DoneReason = c.FinishReason
		}
	}
	fillStats(&out.ollamaStats, u, start)
	writeOllamaJSON(w, http.StatusOK, out)
}

func writeOllamaGenerateResult(w http.ResponseWriter, bw *bufferedWriter, model string, start time.Time) {
	if bw.status >= 400 {
		ollamaError(w, bw.status, extractOpenAIErrorMessage(bw.body.Bytes()))
		return
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Text         string `json:"text"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(bw.body.Bytes(), &resp)
	u, _ := parseUsage(bw.body.Bytes(), false)
	out := struct {
		Model      string    `json:"model"`
		CreatedAt  time.Time `json:"created_at"`
		Response   string    `json:"response"`
		Done       bool      `json:"done"`
		DoneReason string    `json:"done_reason,omitempty"`
		ollamaStats
	}{Model: model, CreatedAt: time.Now().UTC(), Done: true, DoneReason: "stop"}
	if len(resp.Choices) > 0 {
		c := resp.Choices[0]
		if c.Message.Content != "" {
			out.Response = c.Message.Content
		} else {
			out.Response = c.Text
		}
		if c.FinishReason != "" {
			out.DoneReason = c.FinishReason
		}
	}
	fillStats(&out.ollamaStats, u, start)
	writeOllamaJSON(w, http.StatusOK, out)
}

// ollamaField selects which JSON field carries generated text: "message"
// (an object, /api/chat) or "response" (a plain string, /api/generate).
type ollamaField int

const (
	ollamaFieldMessage ollamaField = iota
	ollamaFieldResponse
)

// ollamaStreamAdapter is the ResponseWriter handleProxy writes to for a
// streaming Ollama request. It converts the upstream SSE stream into Ollama
// NDJSON lines in real time (flushing each one), or, if the gateway answers
// with an error before ever streaming (auth, routing, context size, all
// backends failed, ...), buffers that single JSON error body and re-emits it
// in Ollama's flat error shape once the call returns.
type ollamaStreamAdapter struct {
	real  http.ResponseWriter
	model string
	start time.Time
	field ollamaField // output field: "message" (chat) or "response" (generate)
	// completionText reads the delta from /v1/completions' "text" instead of
	// /v1/chat/completions' "delta.content" (set for /api/generate raw=true,
	// which forwards to /v1/completions; /api/chat and /api/generate
	// raw=false both use the chat path).
	completionText bool

	header      http.Header
	status      int
	wroteHeader bool
	isErr       bool

	lineBuf    []byte // partial SSE line carried across Write calls
	raw        []byte // the whole SSE body, for parseUsage once streaming ends
	doneReason string
}

func newOllamaStreamAdapter(w http.ResponseWriter, model string, start time.Time, field ollamaField, completionText bool) *ollamaStreamAdapter {
	return &ollamaStreamAdapter{real: w, model: model, start: start, field: field, completionText: completionText, header: http.Header{}, doneReason: "stop"}
}

func (a *ollamaStreamAdapter) Header() http.Header { return a.header } // scratch: upstream headers (SSE content-type, ...) are not forwarded

func (a *ollamaStreamAdapter) WriteHeader(code int) {
	if a.wroteHeader {
		return
	}
	a.wroteHeader = true
	a.status = code
	if code >= 400 {
		a.isErr = true // real header/status are written once, in finish(), from the buffered error body
		return
	}
	a.real.Header().Set("Content-Type", "application/x-ndjson")
	a.real.WriteHeader(http.StatusOK)
}

func (a *ollamaStreamAdapter) Write(p []byte) (int, error) {
	if !a.wroteHeader {
		a.WriteHeader(http.StatusOK)
	}
	a.raw = append(a.raw, p...)
	if a.isErr {
		return len(p), nil
	}
	a.lineBuf = append(a.lineBuf, p...)
	for {
		i := bytes.IndexByte(a.lineBuf, '\n')
		if i < 0 {
			break
		}
		line := a.lineBuf[:i]
		a.lineBuf = a.lineBuf[i+1:]
		a.handleLine(bytes.TrimRight(line, "\r"))
	}
	return len(p), nil
}

// handleLine parses one SSE line. A "data:" line with a content delta is
// translated and flushed immediately; a stats-only event (llama.cpp's final
// timings/usage line) and "[DONE]" carry no visible content and are read
// later from the accumulated raw body via parseUsage.
func (a *ollamaStreamAdapter) handleLine(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "[DONE]" {
		return
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			Text         string  `json:"text"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &chunk) != nil || len(chunk.Choices) == 0 {
		return
	}
	c := chunk.Choices[0]
	if c.FinishReason != nil && *c.FinishReason != "" {
		a.doneReason = *c.FinishReason
	}
	content := c.Delta.Content
	if a.completionText {
		content = c.Text
	}
	a.emit(content, false, nil)
}

func (a *ollamaStreamAdapter) emit(content string, done bool, extra map[string]any) {
	line := map[string]any{"model": a.model, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "done": done}
	if a.field == ollamaFieldResponse {
		line["response"] = content
	} else {
		line["message"] = map[string]string{"role": "assistant", "content": content}
	}
	for k, v := range extra {
		line[k] = v
	}
	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	b = append(b, '\n')
	_, _ = a.real.Write(b)
	_ = http.NewResponseController(a.real).Flush()
}

// finish emits the final NDJSON line (or the transcoded error) once
// handleProxy has returned. Call exactly once, after g.handleProxy.
func (a *ollamaStreamAdapter) finish() {
	if !a.isErr && len(a.lineBuf) > 0 {
		a.handleLine(a.lineBuf)
		a.lineBuf = nil
	}
	if !a.wroteHeader {
		a.WriteHeader(http.StatusOK)
	}
	if a.isErr {
		ollamaError(a.real, a.status, extractOpenAIErrorMessage(a.raw))
		return
	}
	u, _ := parseUsage(a.raw, true)
	var stats ollamaStats
	fillStats(&stats, u, a.start)
	a.emit("", true, map[string]any{
		"done_reason": a.doneReason, "total_duration": stats.TotalDuration, "load_duration": stats.LoadDuration,
		"prompt_eval_count": stats.PromptEvalCount, "prompt_eval_duration": stats.PromptEvalDuration,
		"eval_count": stats.EvalCount, "eval_duration": stats.EvalDuration,
	})
}
