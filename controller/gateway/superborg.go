package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Super Borg mode (ADR-020): the whole cluster answers as one model. Every
// chat request, whatever its "model", goes to an orchestrator node, which
// may call the delegate tool to run self-contained subtasks on the other
// ready nodes (workers) in parallel; the gateway runs those calls, feeds the
// results back and streams the orchestrator's final answer to the client.

const (
	// KindSuperborg is the kind and id of the single /v1/models entry in
	// Super Borg mode.
	KindSuperborg = "superborg"
	// superborgMaxRounds bounds the delegation rounds per request; the
	// round after the last one is sent without tools, so it must answer.
	superborgMaxRounds = 2
	// superborgWorkerMaxTokens caps a worker's answer: every result token
	// is prompt the orchestrator has to process (~12 tok/s on a phone).
	superborgWorkerMaxTokens = 512
	// superborgResultMaxBytes truncates a worker result fed back to the
	// orchestrator, for the same reason.
	superborgResultMaxBytes = 3000
)

// Superborg configures Super Borg mode; a nil *Superborg is off.
type Superborg struct {
	// Orchestrator is the alias or node id of the orchestrator node; "" or a
	// node that is not ready = the ready node serving the largest model.
	Orchestrator string
	// Thinking lets the orchestrator reason (enable_thinking) before it
	// plans or answers; off is much faster on phones. A client's own
	// chat_template_kwargs win.
	Thinking bool
}

// SetSuperborg switches Super Borg mode on (sb != nil) or off (nil).
// Requests already running are not affected.
func (g *Gateway) SetSuperborg(sb *Superborg) { g.superborg.Store(sb) }

// SuperborgPlan returns the orchestrator and workers a Super Borg request
// would use right now; ok is false when no node is ready.
func (g *Gateway) SuperborgPlan(sb Superborg) (orch Backend, workers []Backend, ok bool) {
	orch, ok = g.pickOrchestrator(sb, nil)
	if !ok {
		return Backend{}, nil, false
	}
	return orch, g.superborgWorkers(orch), true
}

// workerName is how the orchestrator addresses a node.
func workerName(b Backend) string {
	if b.Alias != "" {
		return b.Alias
	}
	return b.NodeID
}

// pickOrchestrator returns the configured orchestrator if it is ready and
// untried, else the untried ready node serving the largest model (catalog
// size, then speed).
func (g *Gateway) pickOrchestrator(sb Superborg, tried map[string]bool) (Backend, bool) {
	var cands []Backend
	for _, b := range g.routable() {
		if tried[b.NodeID] {
			continue
		}
		if sb.Orchestrator != "" && (b.NodeID == sb.Orchestrator || b.Alias == sb.Orchestrator) {
			return b, true
		}
		cands = append(cands, b)
	}
	if len(cands) == 0 {
		return Backend{}, false
	}
	size := func(b Backend) int64 {
		mi, _ := g.modelInfoFor(b.Model)
		return mi.SizeBytes
	}
	sort.SliceStable(cands, func(i, j int) bool {
		si, sj := size(cands[i]), size(cands[j])
		if si != sj {
			return si > sj
		}
		if cands[i].Speed != cands[j].Speed {
			return cands[i].Speed > cands[j].Speed
		}
		return cands[i].NodeID < cands[j].NodeID
	})
	return cands[0], true
}

// superborgWorkers are the ready, not overheating nodes other than the
// orchestrator, sorted by name so the roster prompt stays byte-identical
// (and cached by llama-server) while the cluster does not change.
func (g *Gateway) superborgWorkers(orch Backend) []Backend {
	var out []Backend
	for _, b := range g.routable() {
		if b.NodeID != orch.NodeID && !b.Hot {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return workerName(out[i]) < workerName(out[j]) })
	return out
}

// superborgSystem is the orchestrator's system prompt. Small models follow
// short, concrete rules best; keep it brief, every token is prefill.
func (g *Gateway) superborgSystem(workers []Backend) string {
	var sb strings.Builder
	sb.WriteString("You are SuperBorg, the orchestrator of a cluster of phones, each running a smaller language model. " +
		"You answer the user. With the delegate tool you can run subtasks on worker phones; they run in parallel.\n\nWorkers:\n")
	sb.WriteString(g.roster(workers))
	sb.WriteString("\nRules:\n" +
		"- Answer simple questions yourself, without delegating.\n" +
		"- Delegate only independent, self-contained subtasks (draft, summarize, list, check). " +
		"Workers do not see the conversation: put all needed context into each task.\n" +
		"- Tasks in one call run at the same time: never give a task that needs another task's result. " +
		"Do such a step yourself afterwards, or in a later delegate call with the result included.\n" +
		"- Models under 2B params are only fit for very simple tasks (short lists, yes/no, simple facts in English). " +
		"Give writing, translation and reasoning to the bigger models, or do it yourself. At most one task per worker per call.\n" +
		"- Worker answers are short and often wrong: check them, fix or drop bad parts, then write the final answer yourself, in the user's language.\n")
	if g.jobs.Load() != nil {
		sb.WriteString("- For long, multi-step work (many chapters, parts or items, or a text longer than one answer), " +
			"call start_job instead of delegate: it runs in the background with its own plan, documents and workers.\n")
	}
	return sb.String()
}

// chatTools returns the chat orchestrator's tools: delegate, plus start_job
// when jobs are enabled.
func (g *Gateway) chatTools(workers []Backend) json.RawMessage {
	var tools []any
	_ = json.Unmarshal(delegateTool(workers), &tools)
	if g.jobs.Load() != nil {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name":        "start_job",
			"description": "Start a background job for long, multi-step work; the user follows it in the Jobs view.",
			"parameters": map[string]any{"type": "object", "required": []string{"title"},
				"properties": map[string]any{"title": map[string]any{"type": "string", "description": "short title of the job"}}},
		}})
	}
	return mustJSON(tools)
}

// delegateTool is the chat orchestrator's delegate tool.
func delegateTool(workers []Backend) json.RawMessage {
	names := make([]string, len(workers))
	for i, w := range workers {
		names[i] = workerName(w)
	}
	t := []any{map[string]any{"type": "function", "function": map[string]any{
		"name":        "delegate",
		"description": "Run subtasks on worker phones in parallel and return their answers.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"worker": map[string]any{"type": "string", "enum": names},
						"task":   map[string]any{"type": "string", "description": "complete instructions with all needed context"},
					},
					"required": []string{"worker", "task"},
				},
			}},
			"required": []string{"tasks"},
		},
	}}}
	b, _ := json.Marshal(t)
	return b
}

const workerSystem = "You are a worker in a cluster of phones. Do exactly the task you are given. Be brief and concrete, no preamble."

// superborgDirect reports whether a request goes straight to the
// orchestrator without the delegation loop: raw completions, and clients
// that bring their own tools (coding agents), which must see their tools'
// calls.
func superborgDirect(path string, meta requestMeta) bool {
	return path != "/v1/chat/completions" || (len(meta.Tools) > 0 && string(meta.Tools) != "null")
}

// serveSuperborg runs one Super Borg request; body is the client's request
// (a JSON object) and meta its parsed fields.
func (g *Gateway) serveSuperborg(w http.ResponseWriter, r *http.Request, sb Superborg, body []byte, meta requestMeta, p Principal, reqID string) {
	var req map[string]json.RawMessage
	var msgs []map[string]any
	if json.Unmarshal(body, &req) != nil || json.Unmarshal(mustRaw(req["messages"]), &msgs) != nil || len(msgs) == 0 {
		g.mRejected.WithLabelValues("bad_request").Inc()
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "", "messages must be a non-empty array of message objects")
		return
	}
	orch, ok := g.pickOrchestrator(sb, nil)
	if !ok {
		g.mRejected.WithLabelValues("model_not_found").Inc()
		g.cfg.Usage.Record(UsageEvent{Principal: p.Name, Model: KindSuperborg, Code: "503"})
		openAIError(w, http.StatusServiceUnavailable, "server_error", "model_not_found", "no ready nodes")
		return
	}
	workers := g.superborgWorkers(orch)
	log := g.log.With("request_id", reqID, "mode", KindSuperborg)
	log.Info("superborg request", "orchestrator", orch.NodeID, "workers", len(workers))

	delete(req, "tool_choice")
	delete(req, "stream_options")
	delete(req, "n")
	if _, ok := req["chat_template_kwargs"]; !ok {
		req["chat_template_kwargs"] = mustJSON(map[string]bool{"enable_thinking": sb.Thinking})
	}
	req["stream"] = json.RawMessage("true")
	// extra is the delegation history: assistant tool calls and results.
	var extra []map[string]any
	build := func(final bool) {
		req["messages"] = mustJSON(append(withSystem(msgs, g.superborgSystem(workers)), extra...))
		if final || len(workers) == 0 {
			delete(req, "tools")
		} else {
			req["tools"] = g.chatTools(workers)
		}
	}

	em := newSBEmitter(w, meta.Stream, reqID, g.now().Unix())
	w.Header().Set("X-PhoneBorg-Mode", KindSuperborg)
	w.Header().Set("X-PhoneBorg-Node", orch.NodeID) // the orchestrator; headers go out with the first chunk
	tried := map[string]bool{}
	var total usage
	result := "ok"
	defer func() { g.mSuperborg.WithLabelValues(result).Inc() }()
	for round := 0; ; round++ {
		final := round >= superborgMaxRounds
		build(final)
		out, err := g.orchestrateOnce(r, orch, req, em, p, reqID)
		for errors.Is(err, errRetryable) && !em.started() { // nothing reached the client yet: try another orchestrator
			tried[orch.NodeID] = true
			g.markDown(orch.NodeID)
			log.Warn("orchestrator failed, trying another node", "node_id", orch.NodeID, "err", err)
			if orch, ok = g.pickOrchestrator(sb, tried); !ok {
				break
			}
			workers = g.superborgWorkers(orch)
			w.Header().Set("X-PhoneBorg-Node", orch.NodeID)
			build(final)
			out, err = g.orchestrateOnce(r, orch, req, em, p, reqID)
		}
		if err != nil {
			result = "error"
			log.Warn("superborg request failed", "round", round, "err", err)
			em.fail(http.StatusBadGateway, "superborg: orchestrator failed: "+err.Error())
			return
		}
		total = total.add(out.usage)
		if len(out.calls) == 0 || final || len(workers) == 0 {
			break
		}
		if c, ok := findCall(out.calls, "start_job"); ok {
			em.send("content", g.startJobFromChat(msgs, c, p))
			break
		}
		extra = append(extra, map[string]any{"role": "assistant", "content": out.content, "tool_calls": out.calls})
		for _, c := range out.calls {
			res, u := g.delegate(r, workers, c, em, p, reqID)
			total = total.add(u)
			extra = append(extra, map[string]any{"role": "tool", "tool_call_id": c.ID, "content": res})
		}
	}
	em.finish(total)
}

func findCall(calls []toolCall, name string) (toolCall, bool) {
	for _, c := range calls {
		if c.Function.Name == name {
			return c, true
		}
	}
	return toolCall{}, false
}

// startJobFromChat creates a job from the conversation: its goal is the
// user's last message verbatim (the orchestrator's restatement could drop
// details), its title the one the orchestrator chose. It returns the reply.
func (g *Gateway) startJobFromChat(msgs []map[string]any, c toolCall, p Principal) string {
	var args struct{ Title string }
	_ = json.Unmarshal([]byte(c.Function.Arguments), &args)
	goal := ""
	for _, m := range msgs {
		if role, _ := m["role"].(string); role == "user" {
			if s, ok := m["content"].(string); ok {
				goal = s
			}
		}
	}
	job := g.jobs.Load().Create(args.Title, goal, p.Name)
	return fmt.Sprintf("\n\nStarted job **%s** (`%s`). The cluster works on it in the background: plan, documents, "+
		"progress and the result are in the panel's Jobs view (#/jobs), or `pbctl jobs show %s`. "+
		"Send follow-up instructions there.", job.Title, job.ID, job.ID)
}

// withSystem puts sys before the conversation, merged into the client's own
// leading system message (chat templates accept only one, first).
func withSystem(msgs []map[string]any, sys string) []map[string]any {
	if role, _ := msgs[0]["role"].(string); role == "system" {
		if c, ok := msgs[0]["content"].(string); ok {
			out := append([]map[string]any{}, msgs...)
			out[0] = map[string]any{"role": "system", "content": sys + "\n" + c}
			return out
		}
	}
	return append([]map[string]any{{"role": "system", "content": sys}}, msgs...)
}

// toolCall is an OpenAI tool call, accumulated from stream deltas.
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type orchestration struct {
	content string
	calls   []toolCall
	usage   usage
}

// orchestrateOnce sends one streaming request to the orchestrator through
// forward (inflight, reaping, metrics and usage as for any request),
// relaying its text to the client as it arrives and collecting tool calls.
func (g *Gateway) orchestrateOnce(r *http.Request, orch Backend, req map[string]json.RawMessage, em textSink, p Principal, reqID string) (orchestration, error) {
	sw := &sseCollector{header: http.Header{}, em: em}
	// Idle timeout: a thinking 8B model on a phone can generate for longer
	// than the upstream timeout; it is only stopped when it goes silent.
	err := g.forward(sw, g.internalRequest(r, "/v1/chat/completions", nil), orch, mustJSON(req), true, true, p, reqID)
	if err != nil {
		return orchestration{}, err
	}
	if sw.status != http.StatusOK {
		return orchestration{}, fmt.Errorf("HTTP %d: %s", sw.status, bytes.TrimSpace(sw.errBody.Bytes()))
	}
	if !sw.sawEvent {
		sw.whole(bytes.TrimSpace(sw.line))
	}
	out := orchestration{content: sw.content.String(), usage: sw.usage}
	for _, i := range sw.order {
		c := sw.calls[i]
		if c.ID == "" {
			c.ID = "call_" + newID()
		}
		c.Type = "function"
		out.calls = append(out.calls, *c)
	}
	return out, nil
}

// textSink receives an answer's text as it streams in: field is "content"
// or "reasoning_content". The client emitter and job event logs are sinks.
type textSink interface {
	send(field, text string)
}

// discard is the sink of worker calls, whose text is only collected.
type discard struct{}

func (discard) send(string, string) {}

// sseCollector is the http.ResponseWriter forward writes the
// orchestrator's stream into: it parses the SSE events, relays content and
// reasoning deltas to the client and accumulates tool calls.
type sseCollector struct {
	header  http.Header
	status  int
	em      textSink
	line    []byte
	errBody bytes.Buffer
	content strings.Builder
	calls   map[int]*toolCall
	order   []int
	usage   usage
	// sawEvent is false when the server ignored "stream" and sent one JSON
	// completion (some external engines, ADR-016); whole parses it.
	sawEvent bool
}

func (s *sseCollector) Header() http.Header  { return s.header }
func (s *sseCollector) WriteHeader(code int) { s.status = code }
func (s *sseCollector) Flush()               {}

func (s *sseCollector) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if s.status != http.StatusOK {
		if s.errBody.Len() < 2048 {
			s.errBody.Write(p)
		}
		return len(p), nil
	}
	s.line = append(s.line, p...)
	for {
		i := bytes.IndexByte(s.line, '\n')
		if i < 0 {
			return len(p), nil
		}
		s.event(bytes.TrimSpace(s.line[:i]))
		s.line = s.line[i+1:]
	}
}

func (s *sseCollector) event(line []byte) {
	data, ok := bytes.CutPrefix(line, []byte("data: "))
	if !ok || !bytes.HasPrefix(data, []byte("{")) {
		return
	}
	s.sawEvent = true
	if u, ok := parseUsage(data, false); ok {
		s.usage = u
	}
	var ev struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &ev) != nil || len(ev.Choices) == 0 {
		return
	}
	d := ev.Choices[0].Delta
	if d.ReasoningContent != "" {
		s.em.send("reasoning_content", d.ReasoningContent)
	}
	if d.Content != "" {
		s.content.WriteString(d.Content)
		s.em.send("content", d.Content)
	}
	for _, tc := range d.ToolCalls {
		if s.calls == nil {
			s.calls = map[int]*toolCall{}
		}
		c, ok := s.calls[tc.Index]
		if !ok {
			c = &toolCall{}
			s.calls[tc.Index] = c
			s.order = append(s.order, tc.Index)
		}
		if tc.ID != "" {
			c.ID = tc.ID
		}
		c.Function.Name += tc.Function.Name
		c.Function.Arguments += tc.Function.Arguments
	}
}

// whole handles a non-streaming completion as if it had been streamed.
func (s *sseCollector) whole(body []byte) {
	var v struct {
		Choices []struct {
			Message struct {
				Content          string     `json:"content"`
				ReasoningContent string     `json:"reasoning_content"`
				ToolCalls        []toolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &v) != nil || len(v.Choices) == 0 {
		return
	}
	m := v.Choices[0].Message
	if m.ReasoningContent != "" {
		s.em.send("reasoning_content", m.ReasoningContent)
	}
	if m.Content != "" {
		s.content.WriteString(m.Content)
		s.em.send("content", m.Content)
	}
	for i, c := range m.ToolCalls {
		if s.calls == nil {
			s.calls = map[int]*toolCall{}
		}
		s.calls[i] = &c
		s.order = append(s.order, i)
	}
	s.usage, _ = parseUsage(body, false)
}

// subtask is one delegate tool task.
type subtask struct {
	Worker string `json:"worker"`
	Task   string `json:"task"`
}

// parseSubtasks reads delegate arguments. Small models sometimes drop the
// "tasks" wrapper; a bare {"worker","task"} object is accepted too.
func parseSubtasks(args string) ([]subtask, error) {
	var v struct {
		Tasks []subtask `json:"tasks"`
		subtask
	}
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return nil, err
	}
	if len(v.Tasks) == 0 && v.Task != "" {
		v.Tasks = []subtask{v.subtask}
	}
	if len(v.Tasks) == 0 {
		return nil, errors.New("no tasks")
	}
	return v.Tasks, nil
}

// delegate runs one tool call's subtasks on the workers in parallel and
// returns the tool result text for the orchestrator.
func (g *Gateway) delegate(r *http.Request, workers []Backend, c toolCall, em textSink, p Principal, reqID string) (string, usage) {
	if c.Function.Name != "delegate" {
		return fmt.Sprintf("error: unknown tool %q; the only tool is delegate", c.Function.Name), usage{}
	}
	tasks, err := parseSubtasks(c.Function.Arguments)
	if err != nil {
		return "error: invalid delegate arguments: " + err.Error(), usage{}
	}
	// Assign workers: the requested one if it exists and is free in this
	// call, else the least busy free one; tasks beyond the free workers
	// share them (llama-server queues them).
	used := map[string]bool{}
	assigned := make([]Backend, len(tasks))
	for i, t := range tasks {
		b, ok := findWorker(workers, t.Worker)
		if !ok || used[b.NodeID] {
			b = g.leastBusy(workers, used)
		}
		used[b.NodeID] = true
		assigned[i] = b
		em.send("reasoning_content", fmt.Sprintf("\n→ %s: %s\n", workerName(b), clip(t.Task, 160)))
	}
	results := make([]string, len(tasks))
	usages := make([]usage, len(tasks))
	var wg sync.WaitGroup
	for i, t := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := g.now()
			msgs := []map[string]string{{"role": "system", "content": workerSystem}, {"role": "user", "content": t.Task}}
			b, text, u, err := g.runSubtask(r, workers, assigned[i], msgs, superborgWorkerMaxTokens, p, reqID)
			secs := g.now().Sub(start).Seconds()
			if err != nil {
				g.mDelegations.WithLabelValues(b.NodeID, "error").Inc()
				results[i] = fmt.Sprintf("[%s] error: %v", workerName(b), err)
				em.send("reasoning_content", fmt.Sprintf("← %s: failed after %.1f s: %v\n", workerName(b), secs, err))
				return
			}
			g.mDelegations.WithLabelValues(b.NodeID, "ok").Inc()
			usages[i] = u
			results[i] = fmt.Sprintf("[%s] %s", workerName(b), clip(text, superborgResultMaxBytes))
			em.send("reasoning_content", fmt.Sprintf("← %s: done in %.1f s\n", workerName(b), secs))
		}()
	}
	wg.Wait()
	var total usage
	for _, u := range usages {
		total = total.add(u)
	}
	return strings.Join(results, "\n\n"), total
}

func findWorker(workers []Backend, name string) (Backend, bool) {
	for _, b := range workers {
		if workerName(b) == name || b.NodeID == name {
			return b, true
		}
	}
	return Backend{}, false
}

// leastBusy returns the worker with the fewest in-flight requests that is
// not in used, or the least busy of all if every one is used.
func (g *Gateway) leastBusy(workers []Backend, used map[string]bool) Backend {
	best, bestN := -1, 0
	for pass := 0; pass < 2 && best < 0; pass++ {
		for i, b := range workers {
			if pass == 0 && used[b.NodeID] {
				continue
			}
			if n := g.inflightOf(b.NodeID); best < 0 || n < bestN || (n == bestN && b.Speed > workers[best].Speed) {
				best, bestN = i, n
			}
		}
	}
	return workers[best]
}

// runSubtask sends one task (msgs) to b; if b fails before answering, it
// is retried once on the least busy other worker. It returns the worker that
// answered (or failed last). The call streams with an idle timeout, so a
// slow phone writing a long text is not cut off while it generates.
func (g *Gateway) runSubtask(r *http.Request, workers []Backend, b Backend, msgs []map[string]string, maxTokens int, p Principal, reqID string) (Backend, string, usage, error) {
	body := mustJSON(map[string]any{
		"model":                b.Model,
		"messages":             msgs,
		"max_tokens":           maxTokens,
		"stream":               true,
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
	})
	var lastErr error
	tried := map[string]bool{}
	for attempt := 0; attempt < 2; attempt++ {
		tried[b.NodeID] = true
		sw := &sseCollector{header: http.Header{}, em: discard{}}
		err := g.forward(sw, g.internalRequest(r, "/v1/chat/completions", nil), b, body, true, true, p, reqID)
		if err == nil && sw.status == http.StatusOK {
			if !sw.sawEvent {
				sw.whole(bytes.TrimSpace(sw.line))
			}
			return b, stripThink(sw.content.String()), sw.usage, nil
		}
		if err == nil {
			return b, "", usage{}, fmt.Errorf("HTTP %d: %s", sw.status, clip(extractOpenAIErrorMessage(sw.errBody.Bytes()), 200))
		}
		lastErr = err
		if !errors.Is(err, errRetryable) || r.Context().Err() != nil {
			break
		}
		g.markDown(b.NodeID)
		next := g.leastBusy(workers, tried)
		if tried[next.NodeID] {
			break
		}
		b = next
	}
	return b, "", usage{}, lastErr
}

// stripThink drops a <think> block a reasoning model put into the content.
func stripThink(text string) string {
	if _, after, ok := strings.Cut(text, "</think>"); ok {
		text = after
	}
	return strings.TrimSpace(text)
}

func (u usage) add(o usage) usage {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.CachedTokens += o.CachedTokens
	return u
}

// clip shortens s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only called with JSON-safe values
	}
	return b
}

func mustRaw(r json.RawMessage) []byte {
	if r == nil {
		return []byte("null")
	}
	return r
}

// sbEmitter writes the Super Borg answer to the client: SSE chunks as text
// arrives for streaming clients, one chat.completion at the end otherwise.
// It is safe for concurrent use (workers report progress in parallel).
type sbEmitter struct {
	mu        sync.Mutex
	w         http.ResponseWriter
	stream    bool
	id        string
	created   int64
	begun     bool
	content   strings.Builder
	reasoning strings.Builder
}

func newSBEmitter(w http.ResponseWriter, stream bool, reqID string, created int64) *sbEmitter {
	return &sbEmitter{w: w, stream: stream, id: "chatcmpl-" + reqID, created: created}
}

// started reports whether anything reached the client.
func (e *sbEmitter) started() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.begun
}

// send adds text to the answer's "content" or "reasoning_content".
func (e *sbEmitter) send(field, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.stream {
		if field == "content" {
			e.content.WriteString(text)
		} else {
			e.reasoning.WriteString(text)
		}
		return
	}
	e.chunk(map[string]any{field: text}, nil, nil)
}

// chunk writes one SSE chunk; the caller holds mu.
func (e *sbEmitter) chunk(delta map[string]any, finish *string, u *usage) {
	if !e.begun {
		e.w.Header().Set("Content-Type", "text/event-stream")
		e.w.Header().Set("Cache-Control", "no-cache")
		e.w.WriteHeader(http.StatusOK)
		e.begun = true
	}
	ev := map[string]any{"id": e.id, "object": "chat.completion.chunk", "created": e.created, "model": KindSuperborg,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	if u != nil {
		ev["usage"] = u.openAI()
	}
	fmt.Fprintf(e.w, "data: %s\n\n", mustJSON(ev))
	_ = http.NewResponseController(e.w).Flush()
}

// finish ends a successful answer.
func (e *sbEmitter) finish(u usage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stop := "stop"
	if e.stream {
		e.chunk(map[string]any{}, &stop, &u)
		fmt.Fprint(e.w, "data: [DONE]\n\n")
		_ = http.NewResponseController(e.w).Flush()
		return
	}
	msg := map[string]any{"role": "assistant", "content": e.content.String()}
	if e.reasoning.Len() > 0 {
		msg["reasoning_content"] = e.reasoning.String()
	}
	e.w.Header().Set("Content-Type", "application/json")
	e.w.WriteHeader(http.StatusOK)
	e.begun = true
	_ = json.NewEncoder(e.w).Encode(map[string]any{"id": e.id, "object": "chat.completion", "created": e.created, "model": KindSuperborg,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": stop}}, "usage": u.openAI()})
}

// fail ends the answer with an error: an OpenAI error if nothing was sent
// yet, else a last content chunk saying what went wrong.
func (e *sbEmitter) fail(status int, msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.begun {
		e.begun = true
		openAIError(e.w, status, "server_error", "superborg_failed", msg)
		return
	}
	stop := "stop"
	e.chunk(map[string]any{"content": "\n\n[" + msg + "]"}, &stop, nil)
	fmt.Fprint(e.w, "data: [DONE]\n\n")
	_ = http.NewResponseController(e.w).Flush()
}

func (u usage) openAI() map[string]int {
	return map[string]int{"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens,
		"total_tokens": u.PromptTokens + u.CompletionTokens}
}
