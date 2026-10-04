package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

// MCP tools that let an agent drive the cluster in a loop (ADR-025):
// cluster_map fans independent tasks out to phones, cluster_vote asks several
// phones to judge the same thing, job_wait follows a job without polling.

const (
	mcpMapMaxTasks  = 32
	mcpVoteMaxVotes = 9
	mcpWaitDefault  = 240 * time.Second
	mcpWaitMax      = 30 * time.Minute
)

// mcpWaitEvery is how often job_wait checks the job; tests shorten it.
var mcpWaitEvery = 10 * time.Second

// chatOnce sends one non-streaming chat completion to the gateway and
// returns the answer (without a <think> block) and the node that served it.
func chatOnce(c *client, model, system, prompt string, maxTokens int, temperature float64) (text, node string, err error) {
	msgs := []map[string]string{}
	if system != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": system})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": prompt})
	body, _ := json.Marshal(map[string]any{"model": model, "messages": msgs, "max_tokens": maxTokens, "temperature": temperature,
		"stream": false, "chat_template_kwargs": map[string]bool{"enable_thinking": false}})
	req, err := http.NewRequest(http.MethodPost, c.base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := (&http.Client{Timeout: mcpAskTimeout}).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, errorMessage(raw))
	}
	var v struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || len(v.Choices) == 0 {
		return "", "", fmt.Errorf("unexpected answer: %.200s", raw)
	}
	text = v.Choices[0].Message.Content
	if _, after, ok := strings.Cut(text, "</think>"); ok {
		text = after
	}
	return strings.TrimSpace(text), resp.Header.Get("X-PhoneBorg-Node"), nil
}

// mcpClusterTools are the loop-friendly tools added to mcpTools.
func mcpClusterTools() []mcpTool {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	integer := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	return []mcpTool{
		{Name: "cluster_map", Description: "Run independent tasks on the phone cluster in parallel and return every answer, " +
			"numbered in the order given. Each task must be self-contained (the phones see only that task). Use it to split " +
			"work: write tests for several functions, summarize several files, draft several sections.",
			InputSchema: obj(map[string]any{
				"tasks":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "the tasks, one prompt each (at most 32)"},
				"model":      str(`cluster target, e.g. "pool/code" or "pool/fast" (default "auto")`),
				"system":     str("optional instruction given with every task"),
				"max_tokens": integer("answer length limit per task (default 512)"),
			}, "tasks"), run: mcpMap},
		{Name: "cluster_vote", Description: "Ask several phones the same question and return the votes: each answers with one of " +
			"the options and a one-sentence reason. Use it as a quality gate, e.g. \"Is this paragraph consistent with the plot? " +
			"Rate 1-5\". With a threshold, it also says whether enough voters agreed.",
			InputSchema: obj(map[string]any{
				"question":  str("what to judge, with everything the voters need (the text, the criteria)"),
				"options":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": `allowed answers, e.g. ["yes","no"] or ["1","2","3","4","5"]`},
				"voters":    integer("number of votes (default 5, at most 9)"),
				"model":     str(`cluster target (default "pool/smart")`),
				"threshold": str(`optional pass rule: "4/5" passes when at least 4 of 5 votes give the first option, or for numbers, a value of at least min_score`),
				"min_score": map[string]any{"type": "number", "description": "for numeric options: a vote counts as approving when it is at least this"},
			}, "question", "options"), run: mcpVote},
		{Name: "job_wait", Description: "Wait until a Super Borg job finishes, fails, asks a question or changes its task count, " +
			"or until the timeout; then return its status. Use it to follow a job without asking again and again.",
			InputSchema: obj(map[string]any{
				"id":        str("job id"),
				"timeout_s": integer("longest wait in seconds (default 240, at most 1800)"),
			}, "id"), run: mcpJobWait},
	}
}

func mcpMap(c *client, args map[string]any) (string, error) {
	var tasks []string
	if raw, ok := args["tasks"].([]any); ok {
		for _, t := range raw {
			if s, ok := t.(string); ok && strings.TrimSpace(s) != "" {
				tasks = append(tasks, s)
			}
		}
	}
	if len(tasks) == 0 {
		return "", fmt.Errorf("tasks is required: a list of prompts")
	}
	if len(tasks) > mcpMapMaxTasks {
		return "", fmt.Errorf("at most %d tasks per call, got %d", mcpMapMaxTasks, len(tasks))
	}
	model, _ := argString(args, "model", false)
	if model == "" {
		model = "auto"
	}
	system, _ := argString(args, "system", false)
	maxTokens := 512
	if v, ok := args["max_tokens"].(float64); ok && v > 0 {
		maxTokens = int(v)
	}
	type answer struct {
		text, node string
		err        error
		secs       float64
	}
	answers := make([]answer, len(tasks))
	var wg sync.WaitGroup
	for i, t := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			text, node, err := chatOnce(c, model, system, t, maxTokens, 0.2)
			answers[i] = answer{text, node, err, time.Since(start).Seconds()}
		}()
	}
	wg.Wait()
	var b strings.Builder
	failed := 0
	for i, a := range answers {
		if a.err != nil {
			failed++
			fmt.Fprintf(&b, "### Task %d: failed after %.0f s: %v\n\n", i+1, a.secs, a.err)
			continue
		}
		fmt.Fprintf(&b, "### Task %d (node %s, %.0f s)\n%s\n\n", i+1, a.node, a.secs, a.text)
	}
	fmt.Fprintf(&b, "%d of %d tasks answered on %s.", len(tasks)-failed, len(tasks), model)
	if failed == len(tasks) {
		return "", fmt.Errorf("%s", b.String())
	}
	return b.String(), nil
}

var voteAnswerRE = regexp.MustCompile(`(?im)^\s*\**answer\**\s*[:：]\s*\**\s*(.+?)\s*\**\s*$`)
var voteReasonRE = regexp.MustCompile(`(?im)^\s*\**reason\**\s*[:：]\s*(.+)$`)

// parseVote finds the chosen option in a voter's reply.
func parseVote(reply string, options []string) (string, string) {
	reason := ""
	if m := voteReasonRE.FindStringSubmatch(reply); m != nil {
		reason = strings.TrimSpace(m[1])
	}
	cand := reply
	if m := voteAnswerRE.FindStringSubmatch(reply); m != nil {
		cand = m[1]
	}
	cand = strings.ToLower(strings.Trim(strings.TrimSpace(cand), ".*\"'`"))
	for _, o := range options { // exact match first
		if cand == strings.ToLower(o) {
			return o, reason
		}
	}
	for _, o := range options { // then the first option the answer starts with
		if strings.HasPrefix(cand, strings.ToLower(o)) {
			return o, reason
		}
	}
	return "", reason
}

func mcpVote(c *client, args map[string]any) (string, error) {
	question, err := argString(args, "question", true)
	if err != nil {
		return "", err
	}
	var options []string
	if raw, ok := args["options"].([]any); ok {
		for _, o := range raw {
			if s, ok := o.(string); ok && strings.TrimSpace(s) != "" {
				options = append(options, strings.TrimSpace(s))
			}
		}
	}
	if len(options) < 2 {
		return "", fmt.Errorf("options needs at least two answers")
	}
	voters := 5
	if v, ok := args["voters"].(float64); ok && v > 0 {
		voters = min(int(v), mcpVoteMaxVotes)
	}
	model, _ := argString(args, "model", false)
	if model == "" {
		model = "pool/smart"
	}
	system := "You are one judge of several. Judge independently and strictly. Reply in exactly two lines:\n" +
		"ANSWER: <one of: " + strings.Join(options, ", ") + ">\nREASON: <one sentence>"
	type vote struct{ choice, reason, node, raw string }
	votes := make([]vote, voters)
	errs := make([]error, voters)
	var wg sync.WaitGroup
	for i := range voters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A little temperature, so a phone asked twice does not just
			// repeat itself.
			text, node, err := chatOnce(c, model, system, question, 96, 0.7)
			if err != nil {
				errs[i] = err
				return
			}
			choice, reason := parseVote(text, options)
			votes[i] = vote{choice, reason, node, text}
		}()
	}
	wg.Wait()
	count := map[string]int{}
	valid := 0
	var b strings.Builder
	for i, v := range votes {
		switch {
		case errs[i] != nil:
			fmt.Fprintf(&b, "- voter %d: failed: %v\n", i+1, errs[i])
		case v.choice == "":
			fmt.Fprintf(&b, "- voter %d (node %s): no valid answer: %q\n", i+1, v.node, gatewayClip(v.raw, 120))
		default:
			valid++
			count[v.choice]++
			fmt.Fprintf(&b, "- voter %d (node %s): %s, %s\n", i+1, v.node, v.choice, v.reason)
		}
	}
	if valid == 0 {
		return "", fmt.Errorf("no valid votes:\n%s", b.String())
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return count[keys[i]] > count[keys[j]] || (count[keys[i]] == count[keys[j]] && keys[i] < keys[j])
	})
	var dist []string
	for _, k := range keys {
		dist = append(dist, fmt.Sprintf("%s: %d", k, count[k]))
	}
	head := fmt.Sprintf("Votes (%d valid of %d): %s. Majority: %s.\n", valid, voters, strings.Join(dist, ", "), keys[0])
	if th, _ := argString(args, "threshold", false); th != "" {
		need, of, ok := parseThreshold(th)
		if !ok {
			return "", fmt.Errorf(`threshold must look like "4/5"`)
		}
		approving := count[options[0]]
		if ms, ok := args["min_score"].(float64); ok {
			approving = 0
			for k, n := range count {
				if f, err := strconv.ParseFloat(k, 64); err == nil && f >= ms {
					approving += n
				}
			}
		}
		// Scale the rule to the votes actually cast.
		pass := approving*of >= need*valid
		verdict := "FAIL"
		if pass {
			verdict = "PASS"
		}
		head += fmt.Sprintf("Threshold %s: %s (%d of %d approving).\n", th, verdict, approving, valid)
	}
	return head + b.String(), nil
}

func parseThreshold(s string) (need, of int, ok bool) {
	a, b, found := strings.Cut(strings.TrimSpace(s), "/")
	if !found {
		return 0, 0, false
	}
	need, err1 := strconv.Atoi(strings.TrimSpace(a))
	of, err2 := strconv.Atoi(strings.TrimSpace(b))
	return need, of, err1 == nil && err2 == nil && need > 0 && of >= need
}

func gatewayClip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func mcpJobWait(c *client, args map[string]any) (string, error) {
	id, err := argString(args, "id", true)
	if err != nil {
		return "", err
	}
	timeout := mcpWaitDefault
	if v, ok := args["timeout_s"].(float64); ok && v > 0 {
		timeout = min(time.Duration(v)*time.Second, mcpWaitMax)
	}
	snapshot := func() (gateway.Job, error) {
		raw, err := c.do(http.MethodGet, "/admin/jobs/"+pathEscape(id), nil)
		if err != nil {
			return gateway.Job{}, err
		}
		var j gateway.Job
		return j, json.Unmarshal(raw, &j)
	}
	doneTasks := func(j gateway.Job) int {
		n := 0
		for _, t := range j.Tasks {
			if t.Status == "done" {
				n++
			}
		}
		return n
	}
	first, err := snapshot()
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	for first.Status == gateway.JobRunning || first.Status == gateway.JobQueued {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(min(mcpWaitEvery, time.Until(deadline)+time.Millisecond))
		j, err := snapshot()
		if err != nil {
			return "", err
		}
		if j.Status != first.Status || doneTasks(j) != doneTasks(first) || len(j.Tasks) != len(first.Tasks) {
			break
		}
	}
	status, err := mcpJobStatus(c, map[string]any{"id": id})
	if err != nil {
		return "", err
	}
	return status, nil
}
