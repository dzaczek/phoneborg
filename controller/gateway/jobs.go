package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Super Borg jobs (ADR-021): long, multi-step work that does not fit one chat
// answer. A job runs in the background on the controller. Its state (task
// list, documents, event log) lives on the controller, not in a chat
// history, so worker results are never lost between messages and the
// orchestrator's prompt stays small: every step is a fresh, stateless
// request showing the goal, the task list, document previews and the last
// few steps, and the orchestrator answers with exactly one tool call.

const (
	jobMaxStepsPerRun   = 80   // then the job waits for a user message
	jobWorkerMaxTokens  = 1200 // a worker writes one document, e.g. a chapter
	jobOrchMaxTokens    = 3000 // write_doc texts are written by the orchestrator itself
	jobContextMaxBytes  = 12000
	jobPreviewBytes     = 160
	jobReadMaxBytes     = 6000
	jobRecentSteps      = 6
	jobMaxEvents        = 500
	jobNoNodeRetry      = 15 * time.Second
	jobErrorPause       = 5 * time.Second
	jobMaxErrors        = 5 // consecutive failed steps before the job fails
	jobMaxNoTool        = 3 // steps in a row without a tool call before the job waits
	jobPrincipalDefault = "job"
)

// Job statuses.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobWaiting   = "waiting" // for a user message: a question, or the step budget ran out
	JobDone      = "done"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// JobTask is one entry of a job's task list; Doc is the document it produces.
type JobTask struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	Doc    string `json:"doc,omitempty"`
	Status string `json:"status"` // "todo" or "done"
}

// JobDoc is one document of a job's workspace.
type JobDoc struct {
	Name    string    `json:"name"`
	Text    string    `json:"text"`
	Author  string    `json:"author"` // worker name or "orchestrator"
	Updated time.Time `json:"updated"`
}

// JobEvent is one line of a job's progress log.
type JobEvent struct {
	Seq  int       `json:"seq"`
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // user, think, plan, delegate, worker, doc, read, ask, finish, error, status
	Text string    `json:"text"`
}

// jobStep is what the orchestrator sees of one of its last steps.
type jobStep struct {
	N      int    `json:"n"`
	Tool   string `json:"tool"`
	Args   string `json:"args"`
	Result string `json:"result"`
}

// Job is a Super Borg job and everything it produced.
type Job struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Goal      string    `json:"goal"`
	Messages  []string  `json:"messages"` // later user messages, oldest first
	Principal string    `json:"principal"`
	Status    string    `json:"status"`
	Question  string    `json:"question,omitempty"` // why it waits
	Error     string    `json:"error,omitempty"`
	Tasks     []JobTask `json:"tasks"`
	Docs      []JobDoc  `json:"docs"`
	Result    []string  `json:"result,omitempty"` // documents forming the result, in order
	Summary   string    `json:"summary,omitempty"`
	Steps     int       `json:"steps"`
	Recent    []jobStep `json:"recent"`
	// PlannedAt is len(Messages) when the plan was last set: plan_tasks is
	// offered again only with no plan or after a new user message.
	PlannedAt int `json:"planned_at"`
	// Language of every text, set by plan_tasks and added to every worker
	// task: small models drift into English otherwise.
	Language string `json:"language,omitempty"`
	// FinishAsked is set when finish was refused once for a check against
	// the goal; the next finish is accepted. A new document clears it.
	FinishAsked bool `json:"finish_asked,omitempty"`
	// noTool counts steps in a row without a tool call (not stored).
	noTool  int
	Events  []JobEvent `json:"events"`
	Created time.Time  `json:"created"`
	Updated time.Time  `json:"updated"`
}

// JobSummary is a job in a list, without documents and events.
type JobSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Question  string    `json:"question,omitempty"`
	Steps     int       `json:"steps"`
	TasksDone int       `json:"tasks_done"`
	Tasks     int       `json:"tasks"`
	Docs      int       `json:"docs"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

// ErrJobNotFound is returned for an unknown job id or document name.
var ErrJobNotFound = errors.New("no such job or document")

// Jobs runs and stores Super Borg jobs, one at a time.
type Jobs struct {
	g   *Gateway
	dir string // one <id>.json per job; "" = memory only
	log *slog.Logger

	mu      sync.Mutex
	jobs    map[string]*Job
	running string             // id of the running job
	stop    context.CancelFunc // cancels the running job
	wake    chan struct{}
}

// NewJobs runs jobs and stores them in dir ("" = memory only), starting with
// the jobs loaded by LoadJobs.
func NewJobs(g *Gateway, dir string, loaded map[string]*Job, log *slog.Logger) *Jobs {
	if loaded == nil {
		loaded = map[string]*Job{}
	}
	return &Jobs{g: g, dir: dir, log: log.With("component", "jobs"), jobs: loaded, wake: make(chan struct{}, 1)}
}

// LoadJobs reads the jobs stored in dir; a missing dir is no jobs. Jobs that
// were running when the controller stopped are queued again: each step is
// stateless, so they continue where they were.
func LoadJobs(dir string) (map[string]*Job, error) {
	jobs := map[string]*Job{}
	if dir == "" {
		return jobs, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var job Job
		if err := json.Unmarshal(data, &job); err != nil {
			return nil, fmt.Errorf("%s: %w (move it away to drop the job)", f, err)
		}
		if job.Status == JobRunning {
			job.Status = JobQueued
		}
		jobs[job.ID] = &job
	}
	return jobs, nil
}

// SetJobs enables jobs: the Super Borg chat gets the start_job tool.
func (g *Gateway) SetJobs(j *Jobs) { g.jobs.Store(j) }

// Run executes queued jobs, oldest first, until ctx ends.
func (j *Jobs) Run(ctx context.Context) {
	for {
		if id := j.next(); id != "" {
			j.run(ctx, id)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-j.wake:
		}
	}
}

func (j *Jobs) poke() {
	select {
	case j.wake <- struct{}{}:
	default:
	}
}

// next returns the oldest queued job's id, or "".
func (j *Jobs) next() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var best *Job
	for _, job := range j.jobs {
		if job.Status == JobQueued && (best == nil || job.Created.Before(best.Created)) {
			best = job
		}
	}
	if best == nil {
		return ""
	}
	return best.ID
}

// Create queues a new job.
func (j *Jobs) Create(title, goal, principal string) JobSummary {
	if principal == "" {
		principal = jobPrincipalDefault
	}
	now := j.g.now()
	job := &Job{ID: newID(), Title: strings.TrimSpace(title), Goal: strings.TrimSpace(goal), Principal: principal,
		Status: JobQueued, Tasks: []JobTask{}, Docs: []JobDoc{}, Messages: []string{}, Created: now, Updated: now}
	if job.Title == "" {
		job.Title = clip(strings.Join(strings.Fields(job.Goal), " "), 60)
	}
	j.mu.Lock()
	j.jobs[job.ID] = job
	j.event(job, "user", job.Goal)
	j.saveLocked(job)
	s := summarize(job)
	j.mu.Unlock()
	j.poke()
	return s
}

// List returns all jobs, newest first.
func (j *Jobs) List() []JobSummary {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := []JobSummary{}
	for _, job := range j.jobs {
		out = append(out, summarize(job))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Created.After(out[b].Created) })
	return out
}

// Get returns a copy of a job; documents are returned with previews only.
func (j *Jobs) Get(id string) (Job, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.jobs[id]
	if !ok {
		return Job{}, ErrJobNotFound
	}
	var out Job
	data, _ := json.Marshal(job)
	_ = json.Unmarshal(data, &out)
	for i := range out.Docs {
		out.Docs[i].Text = clip(out.Docs[i].Text, jobPreviewBytes)
	}
	return out, nil
}

// Doc returns one document's full text.
func (j *Jobs) Doc(id, name string) (JobDoc, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.jobs[id]
	if !ok {
		return JobDoc{}, ErrJobNotFound
	}
	if d := findDoc(job, name); d != nil {
		return *d, nil
	}
	return JobDoc{}, ErrJobNotFound
}

// Say adds a user message to a job. A running job sees it from its next
// step on; a waiting, finished or failed job is queued again to act on it.
func (j *Jobs) Say(id, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("empty message")
	}
	j.mu.Lock()
	job, ok := j.jobs[id]
	if !ok {
		j.mu.Unlock()
		return ErrJobNotFound
	}
	job.Messages = append(job.Messages, text)
	j.event(job, "user", text)
	if job.Status != JobRunning {
		job.Status, job.Question, job.Error = JobQueued, "", ""
	}
	j.saveLocked(job)
	j.mu.Unlock()
	j.poke()
	return nil
}

// Cancel stops a job; it keeps its documents and can be resumed with Say.
func (j *Jobs) Cancel(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.jobs[id]
	if !ok {
		return ErrJobNotFound
	}
	if job.Status == JobRunning && j.stop != nil {
		j.stop()
	}
	job.Status = JobCancelled
	j.event(job, "status", "cancelled")
	j.saveLocked(job)
	return nil
}

// Delete cancels and removes a job and its file.
func (j *Jobs) Delete(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.jobs[id]
	if !ok {
		return ErrJobNotFound
	}
	if job.Status == JobRunning && j.stop != nil {
		j.stop()
	}
	delete(j.jobs, id)
	if j.dir != "" {
		if err := os.Remove(filepath.Join(j.dir, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Result assembles the job's result as Markdown: the documents finish named
// (or, before that, the task documents in task order).
func (j *Jobs) Result(id string) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.jobs[id]
	if !ok {
		return "", ErrJobNotFound
	}
	names := job.Result
	if len(names) == 0 {
		for _, t := range job.Tasks {
			if t.Doc != "" && !contains(names, t.Doc) {
				names = append(names, t.Doc)
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", job.Title)
	for _, n := range names {
		if d := findDoc(job, n); d != nil {
			b.WriteString(strings.TrimSpace(d.Text))
			b.WriteString("\n\n")
		}
	}
	return b.String(), nil
}

// run executes steps of one job until it stops being runnable.
func (j *Jobs) run(ctx context.Context, id string) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	j.mu.Lock()
	job := j.jobs[id]
	if job == nil || job.Status != JobQueued {
		j.mu.Unlock()
		return
	}
	job.Status = JobRunning
	j.running, j.stop = id, stop
	j.event(job, "status", "running")
	j.saveLocked(job)
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		j.running, j.stop = "", nil
		j.mu.Unlock()
	}()

	failures := 0
	for n := 0; ; n++ {
		if ctx.Err() != nil {
			return
		}
		j.mu.Lock()
		if job.Status != JobRunning {
			j.mu.Unlock()
			return
		}
		if n >= jobMaxStepsPerRun {
			job.Status = JobWaiting
			job.Question = fmt.Sprintf("Step budget of %d steps reached; send a message (e.g. \"continue\") to go on.", jobMaxStepsPerRun)
			j.event(job, "status", job.Question)
			j.saveLocked(job)
			j.mu.Unlock()
			return
		}
		j.mu.Unlock()
		if err := j.step(ctx, job); err != nil {
			if ctx.Err() != nil {
				return
			}
			pause := jobErrorPause
			j.mu.Lock()
			j.event(job, "error", err.Error())
			if errors.Is(err, errNoNode) {
				pause = jobNoNodeRetry
			} else if failures++; failures >= jobMaxErrors {
				job.Status, job.Error = JobFailed, err.Error()
				j.event(job, "status", fmt.Sprintf("failed after %d errors in a row; send a message to retry", failures))
			}
			j.saveLocked(job)
			j.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(pause):
			}
			continue
		}
		failures = 0
		j.mu.Lock()
		j.saveLocked(job)
		j.mu.Unlock()
	}
}

var errNoNode = errors.New("no ready node to orchestrate; retrying")

// jobSink records the orchestrator's streamed text of one step.
type jobSink struct{ b strings.Builder }

func (s *jobSink) send(_, text string) { s.b.WriteString(text) }

// step runs one orchestrator call and executes its tool calls.
func (j *Jobs) step(ctx context.Context, job *Job) error {
	sb := Superborg{}
	if cur := j.g.superborg.Load(); cur != nil {
		sb = *cur
	}
	orch, ok := j.g.pickOrchestrator(sb, nil)
	if !ok {
		return errNoNode
	}
	workers := j.g.jobWorkers(orch)
	j.mu.Lock()
	prompt := jobPrompt(job)
	principal := job.Principal
	planning := len(job.Tasks) == 0
	allDone := !planning && !slices.ContainsFunc(job.Tasks, func(t JobTask) bool { return t.Status != "done" })
	canPlan := planning || allDone || len(job.Messages) > job.PlannedAt
	j.mu.Unlock()

	req := map[string]json.RawMessage{
		"messages": mustJSON([]map[string]string{
			{"role": "system", "content": j.g.jobSystem(workers)},
			{"role": "user", "content": prompt}}),
		"tools":       jobTools(workers, canPlan),
		"tool_choice": json.RawMessage(`"required"`),
		"stream":      json.RawMessage("true"),
		"max_tokens":  mustJSON(jobOrchMaxTokens),
		// Thinking pays off when planning; in the many delegation steps
		// after that it only costs minutes per step on a phone.
		"chat_template_kwargs": mustJSON(map[string]bool{"enable_thinking": sb.Thinking && planning}),
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Request-Id", job.ID)
	sink := &jobSink{}
	out, err := j.g.orchestrateOnce(r, orch, req, sink, Principal{Name: principal}, job.ID)
	if err != nil {
		return fmt.Errorf("orchestrator %s: %w", workerName(orch), err)
	}
	j.mu.Lock()
	if t := strings.TrimSpace(stripThink(sink.b.String())); t != "" {
		j.event(job, "think", clip(t, 600))
	}
	j.mu.Unlock()
	if len(out.calls) == 0 {
		var none toolCall
		none.Function.Name = "none"
		j.record(job, none, "error: you must call exactly one tool")
		// A model without tool-call support (e.g. Gemma writing
		// "tool_code" as text) never recovers: stop instead of burning
		// the step budget.
		j.mu.Lock()
		if job.noTool++; job.noTool >= jobMaxNoTool {
			job.noTool = 0
			job.Status = JobWaiting
			job.Question = fmt.Sprintf("The orchestrator %s (%s) made no tool call in %d steps in a row; it probably cannot call tools. "+
				"Make a tool-capable node (e.g. one serving Qwen3) the orchestrator, then send a message to continue.",
				workerName(orch), orch.Model, jobMaxNoTool)
			j.event(job, "error", job.Question)
		}
		j.mu.Unlock()
		return nil
	}
	j.mu.Lock()
	job.noTool = 0
	j.mu.Unlock()
	for _, c := range out.calls {
		var res string
		if j.repeats(job, c) {
			res = "error: you just made exactly this call; do the NEXT TASK instead"
		} else {
			res = j.tool(r, job, workers, c, Principal{Name: principal}, canPlan)
		}
		j.record(job, c, res)
		j.mu.Lock()
		stopped := job.Status != JobRunning
		j.mu.Unlock()
		if stopped {
			break
		}
	}
	return nil
}

// record keeps a step's result for the orchestrator's next prompts.
func (j *Jobs) record(job *Job, c toolCall, result string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	tool := c.Function.Name
	job.Steps++
	job.Recent = append(job.Recent, jobStep{N: job.Steps, Tool: tool, Args: c.Function.Arguments, Result: result})
	if len(job.Recent) > jobRecentSteps {
		job.Recent = job.Recent[len(job.Recent)-jobRecentSteps:]
	}
	job.Updated = j.g.now()
	j.g.mJobSteps.WithLabelValues(tool).Inc()
}

// repeats reports whether c is the same call as the previous step (small
// models loop on one tool); read_doc may repeat after a document changed,
// but the cheap rule is enough.
func (j *Jobs) repeats(job *Job, c toolCall) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := len(job.Recent)
	return c.Function.Name != "finish" && // confirming a finish after the goal check repeats it on purpose
		n > 0 && job.Recent[n-1].Tool == c.Function.Name && job.Recent[n-1].Args == c.Function.Arguments
}

// tool executes one orchestrator tool call and returns its result text.
func (j *Jobs) tool(r *http.Request, job *Job, workers []Backend, c toolCall, p Principal, canPlan bool) string {
	var args struct {
		Tasks    json.RawMessage `json:"tasks"`
		Name     string          `json:"name"`
		Text     string          `json:"text"`
		Question string          `json:"question"`
		Summary  string          `json:"summary"`
		Docs     []string        `json:"docs"`
		Language string          `json:"language"`
	}
	if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
		return "error: invalid arguments: " + err.Error()
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	switch c.Function.Name {
	case "plan_tasks":
		if !canPlan {
			return "error: the plan is already set; do the NEXT TASK"
		}
		var tasks []struct{ Title, Doc string }
		if err := json.Unmarshal(args.Tasks, &tasks); err != nil || len(tasks) == 0 {
			return "error: plan_tasks needs tasks: [{title, doc}]"
		}
		// The new plan replaces the tasks not done yet; a document is
		// produced by one task only (small models repeat themselves).
		kept, seen := []JobTask{}, map[string]bool{}
		for _, t := range job.Tasks {
			if t.Status == "done" {
				kept = append(kept, t)
				seen[t.Doc] = true
			}
		}
		added := 0
		for _, t := range tasks {
			doc := docName(t.Doc)
			if strings.TrimSpace(t.Title) == "" || (doc != "" && seen[doc]) {
				continue
			}
			seen[doc] = true
			kept = append(kept, JobTask{Title: strings.TrimSpace(t.Title), Doc: doc, Status: "todo"})
			added++
		}
		for i := range kept {
			kept[i].ID = i + 1
		}
		job.Tasks, job.PlannedAt = kept, len(job.Messages)
		if l := strings.TrimSpace(args.Language); l != "" {
			job.Language = clip(l, 40)
		}
		j.event(job, "plan", fmt.Sprintf("plan set: %d tasks to do", added))
		return fmt.Sprintf("plan set: %d tasks to do (%d tasks were already done)", added, len(kept)-added)
	case "write_doc":
		name := docName(args.Name)
		if name == "" || strings.TrimSpace(args.Text) == "" {
			return "error: write_doc needs name and text"
		}
		j.putDoc(job, name, args.Text, "orchestrator")
		return fmt.Sprintf("saved %s (%d words)", name, words(args.Text))
	case "read_doc":
		d := findDoc(job, docName(args.Name))
		if d == nil {
			return "error: no document " + args.Name
		}
		j.event(job, "read", d.Name)
		return fmt.Sprintf("%s:\n%s", d.Name, clip(d.Text, jobReadMaxBytes))
	case "ask_user":
		job.Status, job.Question = JobWaiting, strings.TrimSpace(args.Question)
		j.event(job, "ask", job.Question)
		return "asked the user: " + job.Question
	case "finish":
		if !job.FinishAsked {
			// Small models stop early (one chapter of twenty): make them
			// compare with the goal once before the job ends.
			job.FinishAsked = true
			return "not finished yet: compare the documents with the GOAL first (every part it asks for, e.g. the number " +
				"of chapters, and their length). If anything is missing, call plan_tasks with the missing tasks. " +
				"If the goal is fully met, call finish again."
		}
		var names []string
		for _, n := range args.Docs {
			if findDoc(job, docName(n)) != nil {
				names = append(names, docName(n))
			}
		}
		job.Status, job.Result, job.Summary = JobDone, names, strings.TrimSpace(args.Summary)
		j.event(job, "finish", job.Summary)
		return "finished"
	case "delegate":
		j.mu.Unlock()
		res := j.delegate(r, job, workers, args.Tasks, p)
		j.mu.Lock()
		return res
	}
	return fmt.Sprintf("error: unknown tool %q", c.Function.Name)
}

// jobSubtask is one delegate task of a job.
type jobSubtask struct {
	Worker  string   `json:"worker"`
	Task    string   `json:"task"`
	Context []string `json:"context"`
	SaveAs  string   `json:"save_as"`
}

// delegate runs subtasks on workers in parallel; each result is saved as a
// document, and only a short preview goes back to the orchestrator.
func (j *Jobs) delegate(r *http.Request, job *Job, workers []Backend, raw json.RawMessage, p Principal) string {
	if len(workers) == 0 {
		return "error: no workers are ready; do the task yourself with write_doc"
	}
	var tasks []jobSubtask
	if err := json.Unmarshal(raw, &tasks); err != nil || len(tasks) == 0 {
		return "error: delegate needs tasks: [{worker, task, context, save_as}]"
	}
	used := map[string]bool{}
	assigned := make([]Backend, len(tasks))
	msgs := make([][]map[string]string, len(tasks))
	j.mu.Lock()
	for i, t := range tasks {
		b, ok := findWorker(workers, t.Worker)
		if !ok || used[b.NodeID] {
			b = j.g.leastBusy(workers, used)
		}
		used[b.NodeID] = true
		assigned[i] = b
		if tasks[i].SaveAs = docName(t.SaveAs); tasks[i].SaveAs == "" {
			tasks[i].SaveAs = fmt.Sprintf("result_%d_%d", job.Steps+1, i+1)
		}
		// Parallel tasks writing the same document would overwrite each
		// other (e.g. three brainstorms); number the later ones.
		for n, base := 2, tasks[i].SaveAs; slices.ContainsFunc(tasks[:i], func(o jobSubtask) bool { return o.SaveAs == tasks[i].SaveAs }); n++ {
			tasks[i].SaveAs = fmt.Sprintf("%s_%d", base, n)
		}
		sys := jobWorkerSystem
		if job.Language != "" {
			sys += " Write in " + job.Language + "."
		}
		msgs[i] = []map[string]string{{"role": "system", "content": sys},
			{"role": "user", "content": workerPrompt(job, t.Context, t.Task)}}
		j.event(job, "delegate", fmt.Sprintf("→ %s: %s → %s", workerName(b), clip(t.Task, 160), tasks[i].SaveAs))
	}
	j.mu.Unlock()

	results := make([]string, len(tasks))
	var wg sync.WaitGroup
	for i := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := j.g.now()
			b, text, _, err := j.g.runSubtask(r, workers, assigned[i], msgs[i], jobWorkerMaxTokens, p, job.ID)
			secs := j.g.now().Sub(start).Seconds()
			j.mu.Lock()
			defer j.mu.Unlock()
			if err == nil && text == "" {
				err = errors.New("empty answer")
			}
			if err != nil {
				j.g.mDelegations.WithLabelValues(b.NodeID, "error").Inc()
				j.event(job, "error", fmt.Sprintf("← %s failed after %.0f s: %v", workerName(b), secs, err))
				results[i] = fmt.Sprintf("%s: error from %s: %v", tasks[i].SaveAs, workerName(b), err)
				return
			}
			j.g.mDelegations.WithLabelValues(b.NodeID, "ok").Inc()
			j.putDoc(job, tasks[i].SaveAs, text, workerName(b))
			j.event(job, "worker", fmt.Sprintf("← %s: %s done in %.0f s (%d words)", workerName(b), tasks[i].SaveAs, secs, words(text)))
			results[i] = fmt.Sprintf("%s ← %s (%d words): %q", tasks[i].SaveAs, workerName(b), words(text), clip(text, jobPreviewBytes))
		}()
	}
	wg.Wait()
	return strings.Join(results, "\n")
}

// putDoc creates or replaces a document and marks tasks producing it done;
// the caller holds mu.
func (j *Jobs) putDoc(job *Job, name, text, author string) {
	now := j.g.now()
	if d := findDoc(job, name); d != nil {
		d.Text, d.Author, d.Updated = text, author, now
	} else {
		job.Docs = append(job.Docs, JobDoc{Name: name, Text: text, Author: author, Updated: now})
	}
	for i := range job.Tasks {
		if job.Tasks[i].Doc == name {
			job.Tasks[i].Status = "done"
		}
	}
	job.FinishAsked = false
	j.event(job, "doc", fmt.Sprintf("%s saved by %s (%d words)", name, author, words(text)))
}

// event appends to the job's log; the caller holds mu.
func (j *Jobs) event(job *Job, kind, text string) {
	seq := 1
	if n := len(job.Events); n > 0 {
		seq = job.Events[n-1].Seq + 1
	}
	job.Events = append(job.Events, JobEvent{Seq: seq, Time: j.g.now(), Kind: kind, Text: text})
	if len(job.Events) > jobMaxEvents {
		job.Events = job.Events[len(job.Events)-jobMaxEvents:]
	}
	job.Updated = j.g.now()
}

// saveLocked writes the job file; the caller holds mu. Errors are logged:
// the job keeps running in memory.
func (j *Jobs) saveLocked(job *Job) {
	if j.dir == "" || j.jobs[job.ID] != job { // deleted meanwhile
		return
	}
	data, err := json.Marshal(job)
	if err == nil {
		if err = os.MkdirAll(j.dir, 0o700); err == nil {
			err = WriteFileAtomic(filepath.Join(j.dir, job.ID+".json"), data, 0o600)
		}
	}
	if err != nil {
		j.log.Warn("cannot store job", "job", job.ID, "err", err)
	}
}

func summarize(job *Job) JobSummary {
	s := JobSummary{ID: job.ID, Title: job.Title, Status: job.Status, Question: job.Question, Steps: job.Steps,
		Tasks: len(job.Tasks), Docs: len(job.Docs), Created: job.Created, Updated: job.Updated}
	for _, t := range job.Tasks {
		if t.Status == "done" {
			s.TasksDone++
		}
	}
	return s
}

// jobMinWorkerParams excludes tiny models from jobs: their texts in
// anything but simple English are unusable, and a job is long-form work.
const jobMinWorkerParams = 1.5e9

// jobWorkers are the Super Borg workers whose catalog parameter count is
// unknown or at least jobMinWorkerParams.
func (g *Gateway) jobWorkers(orch Backend) []Backend {
	var out []Backend
	for _, w := range g.superborgWorkers(orch) {
		mi, _ := g.modelInfoFor(w.Model)
		if n, ok := parseParams(mi.Params); !ok || n >= jobMinWorkerParams {
			out = append(out, w)
		}
	}
	return out
}

// parseParams reads a GGUF size label such as "630M", "4B" or "4.5B".
func parseParams(s string) (float64, bool) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := map[byte]float64{'K': 1e3, 'M': 1e6, 'B': 1e9, 'T': 1e12}
	if s == "" || mult[s[len(s)-1]] == 0 {
		return 0, false
	}
	var n float64
	if _, err := fmt.Sscanf(s[:len(s)-1], "%g", &n); err != nil {
		return 0, false
	}
	return n * mult[s[len(s)-1]], true
}

// Prompts and tools.

func (g *Gateway) jobSystem(workers []Backend) string {
	var sb strings.Builder
	sb.WriteString("You are SuperBorg, the orchestrator of a cluster of phones running small language models. " +
		"You work on a long job step by step. In every step you see the goal, the task list, the documents " +
		"and your last steps, and you call exactly one tool.\n\nWorkers:\n")
	sb.WriteString(g.roster(workers))
	if len(workers) == 0 {
		sb.WriteString("(none ready: write documents yourself)\n")
	}
	sb.WriteString("\nHow to work:\n" +
		"1. If there are no tasks yet, call plan_tasks once with the whole ordered list, covering the WHOLE goal up to the " +
		"finished result (e.g. every chapter it asks for), not only the first step; each task produces one document " +
		"with a short English name. Example for a story in 10 chapters: ideas_1, ideas_2 (workers brainstorm), " +
		"outline (you: plot, characters, one line per chapter), chapter_01 … chapter_10 (workers, one chapter each). " +
		"Each chapter or part is its own task and document: never one task for several chapters.\n" +
		"2. Write short or key documents yourself with write_doc (the plan, the list of characters). " +
		"Give longer, independent texts to workers with delegate, at most one task per worker per call; they run in parallel.\n" +
		"3. Workers see only their task and the documents you list in context: always pass what they need " +
		"(e.g. plan, characters, the previous chapter) and a save_as name. Make each task concrete: content, length, style.\n" +
		"4. Tasks in one delegate call run at the same time: never let one need another's result.\n" +
		"5. Models under 2B params are only fit for very simple tasks.\n" +
		"6. Use read_doc to check a document when unsure; redo a bad one by delegating again with the same save_as.\n" +
		"7. When every task is done, call finish with the documents that form the result, in order.\n" +
		"8. Write everything in the language of the goal. Use ask_user only if the goal is truly unclear.\n")
	return sb.String()
}

// roster lists the workers for a system prompt.
func (g *Gateway) roster(workers []Backend) string {
	var sb strings.Builder
	for _, w := range workers {
		fmt.Fprintf(&sb, "- %s: %s", workerName(w), w.Model)
		var notes []string
		if mi, ok := g.modelInfoFor(w.Model); ok {
			if mi.Params != "" {
				notes = append(notes, mi.Params+" params")
			}
			if len(mi.Tags) > 0 {
				notes = append(notes, strings.Join(mi.Tags, ", "))
			}
		}
		if w.Speed > 0 {
			notes = append(notes, fmt.Sprintf("~%.0f tok/s", w.Speed))
		}
		if len(notes) > 0 {
			fmt.Fprintf(&sb, " (%s)", strings.Join(notes, "; "))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

const jobWorkerSystem = "You are a worker in a cluster of phones and write one part of a larger work. " +
	"Use the documents you are given as context. Do exactly the task: output only the requested text, " +
	"in the language of the task, with no preamble or comments."

// jobPrompt is the user message of one step; the caller holds mu.
func jobPrompt(job *Job) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GOAL:\n%s\n", job.Goal)
	if job.Language != "" {
		fmt.Fprintf(&b, "\nLANGUAGE: write every document and every worker task in %s.\n", job.Language)
	}
	if len(job.Messages) > 0 {
		b.WriteString("\nLATER MESSAGES FROM THE USER (newest last, they override the goal):\n")
		for _, m := range job.Messages {
			fmt.Fprintf(&b, "- %s\n", m)
		}
	}
	b.WriteString("\nTASKS:\n")
	if len(job.Tasks) == 0 {
		b.WriteString("(none yet: start with plan_tasks)\n")
	}
	for _, t := range job.Tasks {
		fmt.Fprintf(&b, "%d. [%s] %s", t.ID, t.Status, t.Title)
		if t.Doc != "" {
			fmt.Fprintf(&b, " → %s", t.Doc)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nDOCUMENTS:\n")
	if len(job.Docs) == 0 {
		b.WriteString("(none yet)\n")
	}
	for _, d := range job.Docs {
		fmt.Fprintf(&b, "- %s (%d words, by %s): %q\n", d.Name, words(d.Text), d.Author, clip(oneLine(d.Text), 100))
	}
	if len(job.Recent) > 0 {
		b.WriteString("\nYOUR LAST STEPS:\n")
		for i, s := range job.Recent {
			res := s.Result
			if i < len(job.Recent)-1 {
				res = clip(res, 400) // only the latest step (e.g. a read_doc) is shown in full
			}
			fmt.Fprintf(&b, "- step %d %s: %s\n", s.N, s.Tool, res)
		}
	}
	for _, t := range job.Tasks {
		if t.Status != "done" {
			fmt.Fprintf(&b, "\nNEXT TASK: %d. %s → %s. Do it now (write_doc or delegate; you may delegate the following "+
				"independent tasks in the same call).", t.ID, t.Title, t.Doc)
			break
		}
	}
	if len(job.Tasks) > 0 && !slices.ContainsFunc(job.Tasks, func(t JobTask) bool { return t.Status != "done" }) {
		b.WriteString("\nAll tasks are done: check the result if needed, then call finish.")
	}
	b.WriteString("\nCall exactly one tool.")
	return b.String()
}

// workerPrompt is a worker's task with its context documents; the caller
// holds mu.
func workerPrompt(job *Job, context []string, task string) string {
	var b strings.Builder
	budget := jobContextMaxBytes
	for _, n := range context {
		d := findDoc(job, docName(n))
		if d == nil || budget <= 0 {
			continue
		}
		text := clip(d.Text, budget)
		budget -= len(text)
		fmt.Fprintf(&b, "### Document: %s\n%s\n\n", d.Name, text)
	}
	fmt.Fprintf(&b, "### Your task\n%s", task)
	return b.String()
}

func jobTools(workers []Backend, canPlan bool) json.RawMessage {
	names := []string{}
	for _, w := range workers {
		names = append(names, workerName(w))
	}
	fn := func(name, desc string, props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": desc,
			"parameters": map[string]any{"type": "object", "properties": props, "required": required}}}
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	strs := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	worker := map[string]any{"type": "string"}
	if len(names) > 0 {
		worker["enum"] = names
	}
	tools := []any{
		fn("plan_tasks", "Set the plan: replaces every task that is not done yet. One task per document, in order.", map[string]any{
			"language": str("language every text must be written in: the language of the goal, e.g. Polish"),
			"tasks": map[string]any{"type": "array",
				"items": map[string]any{"type": "object", "properties": map[string]any{
					"title": str("what to do"), "doc": str("name of the document the task produces")},
					"required": []string{"title", "doc"}}}}, "language", "tasks"),
		fn("delegate", "Run tasks on worker phones in parallel; each result is saved as a document.", map[string]any{"tasks": map[string]any{
			"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
				"worker":  worker,
				"task":    str("complete instructions: what to write, length, style"),
				"context": strs("names of documents the worker needs"),
				"save_as": str("document name for the result")},
				"required": []string{"worker", "task", "save_as"}}}}, "tasks"),
		fn("write_doc", "Write or replace a document yourself.", map[string]any{"name": str("document name"), "text": str("full text")}, "name", "text"),
		fn("read_doc", "Read a document in full (shown in your next step).", map[string]any{"name": str("document name")}, "name"),
		fn("ask_user", "Ask the user a question and wait for the answer.", map[string]any{"question": str("the question")}, "question"),
		fn("finish", "Finish the job when every task is done.", map[string]any{"summary": str("one or two sentences for the user"),
			"docs": strs("documents that form the result, in order")}, "summary", "docs"),
	}
	if !canPlan {
		tools = tools[1:] // plan_tasks is first
	}
	return mustJSON(tools)
}

var (
	docNameRE = regexp.MustCompile(`[^a-z0-9_-]+`)
	// asciiFold drops diacritics of the languages users write goals in,
	// so "Rozdział" becomes "rozdzial", not "rozdzia".
	asciiFold = strings.NewReplacer("ą", "a", "ć", "c", "ę", "e", "ł", "l", "ń", "n", "ó", "o", "ś", "s", "ź", "z", "ż", "z",
		"ä", "a", "ö", "o", "ü", "u", "ß", "ss", "é", "e", "è", "e", "à", "a", "ç", "c", "č", "c", "š", "s", "ž", "z", "ř", "r", "ý", "y", "í", "i", "á", "a")
)

// docName normalises a document name: lower case ASCII, [a-z0-9_-], at most 40.
func docName(s string) string {
	s = asciiFold.Replace(strings.ToLower(strings.TrimSpace(s)))
	s = docNameRE.ReplaceAllString(strings.ReplaceAll(s, " ", "_"), "")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func findDoc(job *Job, name string) *JobDoc {
	for i := range job.Docs {
		if job.Docs[i].Name == name {
			return &job.Docs[i]
		}
	}
	return nil
}

func words(s string) int { return len(strings.Fields(s)) }

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
