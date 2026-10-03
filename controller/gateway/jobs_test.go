package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedOrch streams one tool call per request, from calls in order
// (the last one repeats), and records the request bodies.
type scriptedOrch struct {
	mu    sync.Mutex
	calls []string // `{"name":..., "arguments":...}`
	reqs  []map[string]any
}

func (f *scriptedOrch) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		call := f.calls[min(len(f.reqs), len(f.calls))-1]
		f.mu.Unlock()
		var c struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal([]byte(call), &c)
		w.Header().Set("Content-Type", "text/event-stream")
		ev, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "c", "function": map[string]any{"name": c.Name, "arguments": string(c.Arguments)}}}}}}})
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\ndata: %s\n\ndata: [DONE]\n\n", ev)
	}))
	t.Cleanup(s.Close)
	return s
}

func (f *scriptedOrch) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any{}, f.reqs...)
}

// recordingWorker answers "text from <name>" and records the prompts.
func recordingWorker(t *testing.T, name string, prompts *[]string, mu *sync.Mutex) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		*prompts = append(*prompts, req.Messages[len(req.Messages)-1].Content)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"text from %s\"}}]}\n\ndata: [DONE]\n\n", name)
	}))
	t.Cleanup(s.Close)
	return s
}

func newJobsEnv(t *testing.T, dir string, orch *scriptedOrch, workers ...string) (*Gateway, *Jobs, *[]string) {
	var prompts []string
	var mu sync.Mutex
	backends := []Backend{{NodeID: "o", Alias: "boss", Model: "big", URL: orch.server(t).URL}}
	for _, w := range workers {
		backends = append(backends, Backend{NodeID: w + "-id", Alias: w, Model: "small", URL: recordingWorker(t, w, &prompts, &mu).URL})
	}
	g, _ := newGW(t, AllowAll{}, backends...)
	g.SetModelInfo(func(id string) (ModelInfo, bool) {
		return ModelInfo{SizeBytes: map[string]int64{"big": 9, "small": 1}[id]}, true
	})
	loaded, err := LoadJobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	j := NewJobs(g, dir, loaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.SetJobs(j)
	return g, j, &prompts
}

func runJobs(t *testing.T, j *Jobs) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { j.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func waitStatus(t *testing.T, j *Jobs, id string, want ...string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		job, err := j.Get(id)
		if err == nil && contains(want, job.Status) {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s: status %q, want %v; events %+v", id, job.Status, want, job.Events)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var storyScript = []string{
	`{"name":"plan_tasks","arguments":{"tasks":[{"title":"Plan","doc":"plan"},{"title":"Chapter 1","doc":"Chapter 01"},{"title":"Chapter 2","doc":"chapter_02"}]}}`,
	`{"name":"write_doc","arguments":{"name":"plan","text":"A cat finds a magic key."}}`,
	`{"name":"delegate","arguments":{"tasks":[{"worker":"w1","task":"Write chapter 1","context":["plan"],"save_as":"chapter_01"},{"worker":"w1","task":"Write chapter 2","context":["plan","nope"],"save_as":"chapter_02"}]}}`,
	`{"name":"finish","arguments":{"summary":"Done.","docs":["chapter_01","chapter_02","missing"]}}`,
	`{"name":"finish","arguments":{"summary":"Done.","docs":["chapter_01","chapter_02","missing"]}}`,
}

func TestJobRunsPlanDelegateFinish(t *testing.T) {
	dir := t.TempDir()
	orch := &scriptedOrch{calls: storyScript}
	g, j, prompts := newJobsEnv(t, dir, orch, "w1", "w2")
	runJobs(t, j)
	s := j.Create("", "Napisz bajkę o kocie w 2 rozdziałach", "panel")
	job := waitStatus(t, j, s.ID, JobDone)

	if job.Title != "Napisz bajkę o kocie w 2 rozdziałach" || job.Steps != 5 || job.Summary != "Done." ||
		!strings.Contains(job.Recent[3].Result, "compare the documents with the GOAL") {
		t.Errorf("job %+v", job)
	}
	for _, task := range job.Tasks {
		if task.Status != "done" {
			t.Errorf("task not done: %+v", task)
		}
	}
	if job.Tasks[1].Doc != "chapter_01" {
		t.Errorf("doc name not normalised: %q", job.Tasks[1].Doc)
	}
	// Both tasks named w1; the second went to the free worker.
	d1, _ := j.Doc(s.ID, "chapter_01")
	d2, _ := j.Doc(s.ID, "chapter_02")
	if d1.Text != "text from w1" || d2.Text != "text from w2" || d1.Author != "w1" {
		t.Errorf("docs %+v %+v", d1, d2)
	}
	if len(*prompts) != 2 || !strings.Contains((*prompts)[0], "### Document: plan\nA cat finds a magic key.") ||
		!strings.Contains((*prompts)[0], "### Your task\nWrite chapter") {
		t.Errorf("worker prompts %q", *prompts)
	}
	res, _ := j.Result(s.ID)
	if res != "# Napisz bajkę o kocie w 2 rozdziałach\n\ntext from w1\n\ntext from w2\n\n" {
		t.Errorf("result %q", res)
	}

	// Every step is a fresh two-message request that forces a tool call and
	// shows the state so far.
	reqs := orch.requests()
	last := reqs[4]
	msgs := last["messages"].([]any)
	user := msgs[1].(map[string]any)["content"].(string)
	if len(msgs) != 2 || last["tool_choice"] != "required" ||
		!strings.Contains(user, "2. [done] Chapter 1 → chapter_01") ||
		!strings.Contains(user, `- chapter_02 (3 words, by w2): "text from w2"`) ||
		!strings.Contains(user, "step 3 delegate: chapter_01 ← w1") {
		t.Errorf("step prompt:\n%s", user)
	}
	if !strings.Contains(msgs[0].(map[string]any)["content"].(string), "- w1: small") {
		t.Errorf("system prompt lacks the roster")
	}
	if got := g.mJobSteps.WithLabelValues("delegate"); got == nil {
		t.Error("no step metric")
	}

	// Stored on disk; reloaded by a new controller.
	_, j2, _ := newJobsEnv(t, dir, &scriptedOrch{calls: storyScript})
	if again, err := j2.Get(s.ID); err != nil || again.Status != JobDone || len(again.Docs) != 3 {
		t.Fatalf("reloaded %+v %v", again, err)
	}
	if err := j2.Delete(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, s.ID+".json")); !os.IsNotExist(err) {
		t.Errorf("job file still there: %v", err)
	}
}

func TestJobAskUserAndSay(t *testing.T) {
	orch := &scriptedOrch{calls: []string{
		`{"name":"ask_user","arguments":{"question":"How long?"}}`,
		`{"name":"finish","arguments":{"summary":"ok","docs":[]}}`,
		`{"name":"finish","arguments":{"summary":"ok","docs":[]}}`,
	}}
	_, j, _ := newJobsEnv(t, "", orch, "w1")
	runJobs(t, j)
	s := j.Create("t", "goal", "")
	job := waitStatus(t, j, s.ID, JobWaiting)
	if job.Question != "How long?" || job.Principal != "job" {
		t.Fatalf("job %+v", job)
	}
	if err := j.Say(s.ID, "Short, please"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, j, s.ID, JobDone)
	user := orch.requests()[1]["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "LATER MESSAGES FROM THE USER") || !strings.Contains(user, "- Short, please") {
		t.Errorf("prompt lacks the message:\n%s", user)
	}
	if j.Say("nope", "x") != ErrJobNotFound || j.Say(s.ID, " ") == nil {
		t.Error("bad Say accepted")
	}
}

func TestJobStepBudgetAndCancel(t *testing.T) {
	orch := &scriptedOrch{calls: []string{`{"name":"read_doc","arguments":{"name":"nothing"}}`}}
	_, j, _ := newJobsEnv(t, "", orch)
	runJobs(t, j)
	s := j.Create("loop", "goal", "")
	job := waitStatus(t, j, s.ID, JobWaiting)
	if job.Steps != jobMaxStepsPerRun || !strings.Contains(job.Question, "Step budget") {
		t.Fatalf("job %+v", job)
	}
	if err := j.Cancel(s.ID); err != nil {
		t.Fatal(err)
	}
	if job, _ := j.Get(s.ID); job.Status != JobCancelled {
		t.Errorf("status %s", job.Status)
	}
}

func TestJobRequeuedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	_, j, _ := newJobsEnv(t, dir, &scriptedOrch{calls: storyScript})
	s := j.Create("t", "goal", "") // not run: no runner
	path := filepath.Join(dir, s.ID+".json")
	data, _ := os.ReadFile(path)
	_ = os.WriteFile(path, []byte(strings.Replace(string(data), `"status":"queued"`, `"status":"running"`, 1)), 0o600)
	_, j2, _ := newJobsEnv(t, dir, &scriptedOrch{calls: storyScript}, "w1", "w2")
	if job, _ := j2.Get(s.ID); job.Status != JobQueued {
		t.Fatalf("status %s", job.Status)
	}
	runJobs(t, j2)
	waitStatus(t, j2, s.ID, JobDone)
}

func TestSuperborgChatStartsJob(t *testing.T) {
	orch := &scriptedOrch{calls: []string{`{"name":"start_job","arguments":{"title":"Bajka"}}`}}
	g, j, _ := newJobsEnv(t, "", orch, "w1")
	g.SetSuperborg(&Superborg{})
	mux := http.NewServeMux()
	g.Register(mux)
	w := post(mux, `{"model":"x","messages":[{"role":"user","content":"Napisz bajkę w 20 rozdziałach"}]}`)
	list := j.List()
	if w.Code != 200 || len(list) != 1 || list[0].Title != "Bajka" || !strings.Contains(w.Body.String(), "Started job **Bajka** (`"+list[0].ID) {
		t.Fatalf("status %d, jobs %+v: %s", w.Code, list, w.Body)
	}
	job, _ := j.Get(list[0].ID)
	if job.Goal != "Napisz bajkę w 20 rozdziałach" {
		t.Errorf("goal %q", job.Goal)
	}
	tools := fmt.Sprint(orch.requests()[0]["tools"])
	if !strings.Contains(tools, "start_job") || !strings.Contains(tools, "delegate") {
		t.Errorf("tools %s", tools)
	}
}

func TestDocName(t *testing.T) {
	for in, want := range map[string]string{"Chapter 01": "chapter_01", " plan ": "plan", "Rozdział 3!": "rozdzial_3", "Żółć": "zolc", "": ""} {
		if got := docName(in); got != want {
			t.Errorf("docName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlanTasksReplacesUndoneTasks(t *testing.T) {
	orch := &scriptedOrch{calls: []string{
		`{"name":"plan_tasks","arguments":{"tasks":[{"title":"Plan","doc":"plan"},{"title":"Ch 1","doc":"chapter_01"},{"title":"again","doc":"chapter_01"}]}}`,
		`{"name":"write_doc","arguments":{"name":"plan","text":"p"}}`,
		`{"name":"plan_tasks","arguments":{"tasks":[{"title":"Plan again","doc":"plan"},{"title":"Ch 1 new","doc":"chapter_01"},{"title":"Ch 2","doc":"chapter_02"}]}}`,
		`{"name":"ask_user","arguments":{"question":"ok?"}}`,
		`{"name":"plan_tasks","arguments":{"tasks":[{"title":"Plan again","doc":"plan"},{"title":"Ch 1 new","doc":"chapter_01"},{"title":"Ch 2","doc":"chapter_02"}]}}`,
		`{"name":"ask_user","arguments":{"question":"ok now?"}}`,
	}}
	g, j, _ := newJobsEnv(t, "", orch)
	g.SetSuperborg(&Superborg{Thinking: true})
	runJobs(t, j)
	s := j.Create("t", "goal", "")
	job := waitStatus(t, j, s.ID, JobWaiting)
	// Without a new message the plan cannot change: plan_tasks is not even offered.
	if len(job.Tasks) != 2 || !strings.Contains(job.Recent[2].Result, "the plan is already set") {
		t.Fatalf("replanned without a message: %+v %+v", job.Tasks, job.Recent)
	}
	reqs := orch.requests()
	if strings.Contains(fmt.Sprint(reqs[2]["tools"]), "plan_tasks") || !strings.Contains(fmt.Sprint(reqs[0]["tools"]), "plan_tasks") {
		t.Error("plan_tasks offered when it should not be, or not offered at first")
	}
	user := reqs[2]["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "NEXT TASK: 2. Ch 1 → chapter_01. Do it now") {
		t.Errorf("prompt lacks the next task:\n%s", user)
	}
	_ = j.Say(s.ID, "two chapters please")
	for job.Question != "ok now?" {
		job = waitStatus(t, j, s.ID, JobWaiting)
	}
	var got []string
	for _, task := range job.Tasks {
		got = append(got, fmt.Sprintf("%d %s %s %s", task.ID, task.Status, task.Doc, task.Title))
	}
	if strings.Join(got, "|") != "1 done plan Plan|2 todo chapter_01 Ch 1 new|3 todo chapter_02 Ch 2" {
		t.Errorf("tasks %q", got)
	}
	// Thinking only while there is no plan yet.
	reqs = orch.requests()
	think := func(i int) any { return reqs[i]["chat_template_kwargs"].(map[string]any)["enable_thinking"] }
	if think(0) != true || think(1) != false {
		t.Errorf("thinking per step: %v %v", think(0), think(1))
	}
}

func TestJobRepeatedCallIsRejected(t *testing.T) {
	orch := &scriptedOrch{calls: []string{
		`{"name":"write_doc","arguments":{"name":"a","text":"x"}}`,
		`{"name":"write_doc","arguments":{"name":"a","text":"x"}}`,
		`{"name":"ask_user","arguments":{"question":"q"}}`,
	}}
	_, j, _ := newJobsEnv(t, "", orch)
	runJobs(t, j)
	s := j.Create("t", "goal", "")
	job := waitStatus(t, j, s.ID, JobWaiting)
	if !strings.Contains(job.Recent[1].Result, "you just made exactly this call") {
		t.Errorf("recent %+v", job.Recent)
	}
}

func TestJobLanguageDuplicatesAndTinyWorkers(t *testing.T) {
	orch := &scriptedOrch{calls: []string{
		`{"name":"plan_tasks","arguments":{"language":"Polish","tasks":[{"title":"Ideas","doc":"ideas"}]}}`,
		`{"name":"delegate","arguments":{"tasks":[{"worker":"w1","task":"ideas","save_as":"ideas"},{"worker":"w2","task":"ideas","save_as":"ideas"}]}}`,
		`{"name":"ask_user","arguments":{"question":"q"}}`,
	}}
	g, j, prompts := newJobsEnv(t, "", orch, "w1", "w2")
	runJobs(t, j)
	s := j.Create("t", "Napisz bajkę", "")
	job := waitStatus(t, j, s.ID, JobWaiting)
	names := map[string]bool{}
	for _, d := range job.Docs {
		names[d.Name] = true
	}
	if job.Language != "Polish" || len(job.Docs) != 2 || !names["ideas"] || !names["ideas_2"] { // workers finish in any order
		t.Fatalf("language %q, docs %+v", job.Language, job.Docs)
	}
	user := orch.requests()[1]["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "LANGUAGE: write every document and every worker task in Polish.") {
		t.Errorf("prompt:\n%s", user)
	}
	if len(*prompts) != 2 {
		t.Errorf("worker prompts %q", *prompts)
	}
	// A worker known to be 0.5B is left out of jobs.
	g.SetModelInfo(func(id string) (ModelInfo, bool) {
		return ModelInfo{Params: map[string]string{"big": "8B", "small": "630M"}[id]}, true
	})
	o, _ := g.pickOrchestrator(Superborg{}, nil)
	if w := g.jobWorkers(o); len(w) != 0 {
		t.Errorf("tiny workers offered: %v", w)
	}
}

func TestParseParams(t *testing.T) {
	for in, want := range map[string]float64{"630M": 630e6, "4B": 4e9, "4.5b": 4.5e9, "mini": 0, "": 0} {
		if got, _ := parseParams(in); got != want {
			t.Errorf("parseParams(%q) = %g, want %g", in, got, want)
		}
	}
}

func TestJobWaitsWhenOrchestratorCannotCallTools(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"```tool_code delegate```\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(s.Close)
	g, _ := newGW(t, AllowAll{}, Backend{NodeID: "o", Alias: "mi8", Model: "gemma", URL: s.URL})
	j := NewJobs(g, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runJobs(t, j)
	id := j.Create("t", "goal", "").ID
	job := waitStatus(t, j, id, JobWaiting)
	if job.Steps != jobMaxNoTool || !strings.Contains(job.Question, "mi8 (gemma) made no tool call in 3 steps") {
		t.Fatalf("steps %d, question %q", job.Steps, job.Question)
	}
}
