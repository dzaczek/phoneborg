package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dzaczek/phoneborg/controller"
	"github.com/dzaczek/phoneborg/controller/gateway"
)

// pbctl mcp: a Model Context Protocol server on stdin/stdout (ADR-025), so an
// agent such as opencode can use the cluster as tools: ask it a question,
// start Super Borg jobs and follow them. Messages are newline-delimited
// JSON-RPC 2.0; nothing but protocol messages may be written to stdout.

// mcpProtocolVersions are the MCP revisions this server speaks, newest
// first; the newest is offered when the client asks for another one.
var mcpProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// osStdin is where pbctl mcp reads requests; tests replace it.
var osStdin io.Reader = os.Stdin

// mcpAskTimeout bounds one ask_cluster call: a phone may process a prompt
// and generate for many minutes.
const mcpAskTimeout = 35 * time.Minute

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

// mcpTool is one tool: its schema for tools/list and its handler.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	run         func(c *client, args map[string]any) (string, error)
}

func mcpCmd(c *client, o *out, args []string) error {
	if len(args) != 0 {
		return errUsage
	}
	return serveMCP(c, osStdin, o.w)
}

// serveMCP answers requests from in until it ends.
func serveMCP(c *client, in io.Reader, w io.Writer) error {
	tools := mcpTools()
	byName := map[string]mcpTool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	enc := json.NewEncoder(w) // one message per line
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req mcpRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &mcpError{Code: -32700, Message: "parse error: " + err.Error()}})
			continue
		}
		if len(req.ID) == 0 { // a notification, e.g. notifications/initialized
			continue
		}
		resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			version := mcpProtocolVersions[0]
			for _, v := range mcpProtocolVersions {
				if v == p.ProtocolVersion {
					version = v
				}
			}
			resp.Result = map[string]any{
				"protocolVersion": version,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "phoneborg", "version": "1"},
				"instructions": "PhoneBorg is a cluster of phones running small language models. Use ask_cluster for small, " +
					"self-contained tasks; use job_start for long multi-step work (it runs in the background for minutes " +
					"to hours) and follow it with job_status; fetch the text with job_result.",
			}
		case "ping":
			resp.Result = map[string]any{}
		case "tools/list":
			resp.Result = map[string]any{"tools": tools}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				resp.Error = &mcpError{Code: -32602, Message: "invalid params: " + err.Error()}
				break
			}
			t, ok := byName[p.Name]
			if !ok {
				resp.Error = &mcpError{Code: -32602, Message: "unknown tool " + p.Name}
				break
			}
			text, err := t.run(c, p.Arguments)
			if err != nil {
				// Tool failures are results the model can read, not protocol errors.
				resp.Result = map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}
				break
			}
			resp.Result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		default:
			resp.Error = &mcpError{Code: -32601, Message: "method not found: " + req.Method}
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func mcpTools() []mcpTool {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	obj := func(props map[string]any, required ...string) map[string]any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	return []mcpTool{
		{Name: "cluster_status", Description: "List the phones (model, speed, state) and pools of the PhoneBorg cluster.",
			InputSchema: obj(map[string]any{}), run: mcpClusterStatus},
		{Name: "ask_cluster", Description: "Send one self-contained prompt to the phone cluster and return the answer. " +
			"Slow (phones generate a few tokens per second): keep prompts short and tasks small.",
			InputSchema: obj(map[string]any{
				"prompt":     str("the complete task, with all context it needs"),
				"model":      str(`cluster target: "auto" (default), "pool/<name>" or "node/<alias>"`),
				"max_tokens": map[string]any{"type": "integer", "description": "answer length limit (default 512)"},
			}, "prompt"), run: mcpAsk},
		{Name: "job_start", Description: "Start a Super Borg job: long multi-step work (e.g. a document in many parts) " +
			"that the cluster plans and writes in the background. Returns the job id.",
			InputSchema: obj(map[string]any{
				"goal":  str("what to produce, as precisely as possible"),
				"title": str("short title (optional)"),
				"pool":  str("Super Borg pool to run on (optional; default the first one)"),
			}, "goal"), run: mcpJobStart},
		{Name: "job_status", Description: "Status of a job: tasks done, documents, the question it waits on, latest events.",
			InputSchema: obj(map[string]any{"id": str("job id")}, "id"), run: mcpJobStatus},
		{Name: "job_message", Description: "Send an instruction to a job; resumes it if it waits, is done or cancelled.",
			InputSchema: obj(map[string]any{"id": str("job id"), "text": str("the instruction")}, "id", "text"), run: mcpJobMessage},
		{Name: "job_result", Description: "The job's assembled result as Markdown.",
			InputSchema: obj(map[string]any{"id": str("job id")}, "id"), run: mcpJobResult},
		{Name: "jobs_list", Description: "All jobs with status and progress.",
			InputSchema: obj(map[string]any{}), run: mcpJobsList},
	}
}

func argString(args map[string]any, name string, required bool) (string, error) {
	v, _ := args[name].(string)
	v = strings.TrimSpace(v)
	if v == "" && required {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}

func mcpClusterStatus(c *client, _ map[string]any) (string, error) {
	raw, err := c.do(http.MethodGet, "/admin/nodes", nil)
	if err != nil {
		return "", err
	}
	var nodes []controller.AdminNode
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("Phones:\n")
	for _, n := range nodes {
		name, model, tps := n.ID, "-", 0.0
		if n.Alias != "" {
			name = n.Alias
		}
		if hb := n.LastHeartbeat; hb != nil && hb.Runtime != nil {
			model, tps = hb.Runtime.Model, hb.Runtime.GenTPS
		}
		fmt.Fprintf(&b, "- %s: %s, %s, %.1f tok/s\n", name, n.State, model, tps)
	}
	raw, err = c.do(http.MethodGet, "/admin/pools", nil)
	if err == nil {
		var ps controller.Pools
		if json.Unmarshal(raw, &ps) == nil && len(ps.Pools) > 0 {
			b.WriteString("Pools:\n")
			for _, p := range ps.Pools {
				fmt.Fprintf(&b, "- pool/%s: routing %s, %d eligible\n", p.Name, p.Routing, eligibleNodes(p.Members))
			}
		}
	}
	return b.String(), nil
}

func mcpAsk(c *client, args map[string]any) (string, error) {
	prompt, err := argString(args, "prompt", true)
	if err != nil {
		return "", err
	}
	model, _ := argString(args, "model", false)
	if model == "" {
		model = "auto"
	}
	maxTokens := 512
	if v, ok := args["max_tokens"].(float64); ok && v > 0 {
		maxTokens = int(v)
	}
	body := map[string]any{"model": model, "max_tokens": maxTokens, "stream": false,
		"messages": []map[string]string{{"role": "user", "content": prompt}}}
	slow := *c
	slow.http = &http.Client{Timeout: mcpAskTimeout}
	raw, err := slow.request(http.MethodPost, "/v1/chat/completions", body, c.apiKey)
	if err != nil {
		return "", err
	}
	var v struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || len(v.Choices) == 0 {
		return "", fmt.Errorf("unexpected answer: %.200s", raw)
	}
	text := v.Choices[0].Message.Content
	if _, after, ok := strings.Cut(text, "</think>"); ok {
		text = after
	}
	return strings.TrimSpace(text), nil
}

func mcpJobStart(c *client, args map[string]any) (string, error) {
	goal, err := argString(args, "goal", true)
	if err != nil {
		return "", err
	}
	title, _ := argString(args, "title", false)
	pool, _ := argString(args, "pool", false)
	raw, err := c.do(http.MethodPost, "/admin/jobs", controller.JobCreate{Title: title, Goal: goal, Pool: pool})
	if err != nil {
		return "", err
	}
	var j gateway.JobSummary
	if err := json.Unmarshal(raw, &j); err != nil {
		return "", err
	}
	where := j.Pool
	if where == "" {
		where = "the whole cluster"
	}
	return fmt.Sprintf("Started job %s (%q) on %s. It runs in the background; check it with job_status id=%s.", j.ID, j.Title, where, j.ID), nil
}

func mcpJobStatus(c *client, args map[string]any) (string, error) {
	id, err := argString(args, "id", true)
	if err != nil {
		return "", err
	}
	raw, err := c.do(http.MethodGet, "/admin/jobs/"+pathEscape(id), nil)
	if err != nil {
		return "", err
	}
	var j gateway.Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return "", err
	}
	done := 0
	for _, t := range j.Tasks {
		if t.Status == "done" {
			done++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Job %s %q: %s, %d steps, %d/%d tasks done.\n", j.ID, j.Title, j.Status, j.Steps, done, len(j.Tasks))
	if j.Question != "" {
		fmt.Fprintf(&b, "Waiting: %s\n", j.Question)
	}
	if j.Error != "" {
		fmt.Fprintf(&b, "Error: %s\n", j.Error)
	}
	if j.Summary != "" {
		fmt.Fprintf(&b, "Summary: %s\n", j.Summary)
	}
	names := make([]string, 0, len(j.Docs))
	for _, d := range j.Docs {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	fmt.Fprintf(&b, "Documents: %s\n", strings.Join(names, ", "))
	ev := j.Events
	if len(ev) > 8 {
		ev = ev[len(ev)-8:]
	}
	b.WriteString("Latest events:\n")
	for _, e := range ev {
		fmt.Fprintf(&b, "- %s %s: %s\n", e.Time.Format(time.TimeOnly), e.Kind, strings.Join(strings.Fields(e.Text), " "))
	}
	return b.String(), nil
}

func mcpJobMessage(c *client, args map[string]any) (string, error) {
	id, err := argString(args, "id", true)
	if err != nil {
		return "", err
	}
	text, err := argString(args, "text", true)
	if err != nil {
		return "", err
	}
	if _, err := c.do(http.MethodPost, "/admin/jobs/"+pathEscape(id)+"/messages", controller.JobMessage{Text: text}); err != nil {
		return "", err
	}
	return "Message sent to job " + id + "; the orchestrator sees it from its next step.", nil
}

func mcpJobResult(c *client, args map[string]any) (string, error) {
	id, err := argString(args, "id", true)
	if err != nil {
		return "", err
	}
	raw, err := c.do(http.MethodGet, "/admin/jobs/"+pathEscape(id)+"/result", nil)
	return string(raw), err
}

func mcpJobsList(c *client, _ map[string]any) (string, error) {
	raw, err := c.do(http.MethodGet, "/admin/jobs", nil)
	if err != nil {
		return "", err
	}
	var l controller.JobList
	if err := json.Unmarshal(raw, &l); err != nil {
		return "", err
	}
	if len(l.Jobs) == 0 {
		return "No jobs.", nil
	}
	var b strings.Builder
	for _, j := range l.Jobs {
		fmt.Fprintf(&b, "- %s %q: %s, %d/%d tasks, %d steps\n", j.ID, j.Title, j.Status, j.TasksDone, j.Tasks, j.Steps)
	}
	return b.String(), nil
}
