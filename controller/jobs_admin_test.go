package controller

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

func TestJobsAdmin(t *testing.T) {
	e := newRoutingEnv(t, "", "a")
	var job gateway.JobSummary
	e.admin(http.MethodPost, "/admin/jobs", `{"goal":" "}`, 400, nil)
	e.admin(http.MethodPost, "/admin/jobs", `{"goal":"x","bogus":1}`, 400, nil)
	e.admin(http.MethodPost, "/admin/jobs", `{"title":"Story","goal":"Write a story"}`, 201, &job)
	if job.Title != "Story" || job.Status != gateway.JobQueued {
		t.Fatalf("created %+v", job)
	}
	var list JobList
	e.admin(http.MethodGet, "/admin/jobs", "", 200, &list)
	if len(list.Jobs) != 1 || list.Jobs[0].ID != job.ID {
		t.Fatalf("list %+v", list)
	}
	var full gateway.Job
	e.admin(http.MethodPost, "/admin/jobs/"+job.ID+"/messages", `{"text":"shorter"}`, 200, &full)
	if full.Goal != "Write a story" || full.Principal != PanelPrincipal || len(full.Messages) != 1 {
		t.Fatalf("job %+v", full)
	}
	e.admin(http.MethodGet, "/admin/jobs/nope", "", 404, nil)
	e.admin(http.MethodGet, "/admin/jobs/"+job.ID+"/docs/plan", "", 404, nil)
	w := e.do(http.MethodGet, "/admin/jobs/"+job.ID+"/result", "", testToken)
	if w.Code != 200 || w.Body.String() != "# Story\n\n" || !strings.Contains(w.Header().Get("Content-Disposition"), "job-"+job.ID+".md") {
		t.Fatalf("result %d %q", w.Code, w.Body)
	}
	e.admin(http.MethodPost, "/admin/jobs/"+job.ID+"/cancel", "", 200, &full)
	if full.Status != gateway.JobCancelled {
		t.Errorf("status %s", full.Status)
	}
	if m := e.do(http.MethodGet, "/metrics", "", "").Body.String(); !strings.Contains(m, `phoneborg_superborg_jobs{status="cancelled"} 1`) {
		t.Errorf("metrics lack the job")
	}
	e.admin(http.MethodDelete, "/admin/jobs/"+job.ID, "", 204, nil)
	e.admin(http.MethodDelete, "/admin/jobs/"+job.ID, "", 404, nil)
}
