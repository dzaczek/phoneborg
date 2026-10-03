package controller

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

// Admin API of Super Borg jobs (ADR-021): long, multi-step work the
// orchestrator runs in the background with a task list and documents.

// JobsOptions configures job storage.
type JobsOptions struct {
	Dir   string                  // one file per job; "" = memory only
	State map[string]*gateway.Job // jobs loaded with gateway.LoadJobs
}

// JobCreate is the body of POST /admin/jobs.
type JobCreate struct {
	Title string `json:"title"`
	Goal  string `json:"goal"`
}

// JobMessage is the body of POST /admin/jobs/{id}/messages.
type JobMessage struct {
	Text string `json:"text"`
}

// JobList is the response of GET /admin/jobs.
type JobList struct {
	Jobs []gateway.JobSummary `json:"jobs"`
}

func (s *Server) registerJobsAdmin(add func(pattern, action string, fn http.HandlerFunc)) {
	add("GET /admin/jobs", "jobs_list", s.adminJobs)
	add("POST /admin/jobs", "job_create", s.adminCreateJob)
	add("GET /admin/jobs/{id}", "job_get", s.adminJob)
	add("GET /admin/jobs/{id}/docs/{name}", "job_doc", s.adminJobDoc)
	add("GET /admin/jobs/{id}/result", "job_result", s.adminJobResult)
	add("POST /admin/jobs/{id}/messages", "job_message", s.adminJobMessage)
	add("POST /admin/jobs/{id}/cancel", "job_cancel", s.adminJobCancel)
	add("DELETE /admin/jobs/{id}", "job_delete", s.adminDeleteJob)
}

func (s *Server) adminJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, JobList{Jobs: s.jobs.List()})
}

func (s *Server) adminCreateJob(w http.ResponseWriter, r *http.Request) {
	var c JobCreate
	if !decodeStrict(w, r, &c) {
		return
	}
	if strings.TrimSpace(c.Goal) == "" {
		httpError(w, http.StatusBadRequest, "goal is required")
		return
	}
	job := s.jobs.Create(c.Title, c.Goal, PanelPrincipal)
	s.audit(r, "job_create", nil, "job", job.ID, "title", job.Title)
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) adminJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.jobs.Get(r.PathValue("id"))
	if err != nil {
		jobError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) adminJobDoc(w http.ResponseWriter, r *http.Request) {
	d, err := s.jobs.Doc(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		jobError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) adminJobResult(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	md, err := s.jobs.Result(id)
	if err != nil {
		jobError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="job-%s.md"`, id))
	_, _ = w.Write([]byte(md))
}

func (s *Server) adminJobMessage(w http.ResponseWriter, r *http.Request) {
	var m JobMessage
	if !decodeStrict(w, r, &m) {
		return
	}
	id := r.PathValue("id")
	err := s.jobs.Say(id, m.Text)
	s.audit(r, "job_message", err, "job", id)
	if err != nil {
		jobError(w, err)
		return
	}
	s.adminJob(w, r)
}

func (s *Server) adminJobCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.jobs.Cancel(id)
	s.audit(r, "job_cancel", err, "job", id)
	if err != nil {
		jobError(w, err)
		return
	}
	s.adminJob(w, r)
}

func (s *Server) adminDeleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.jobs.Delete(id)
	s.audit(r, "job_delete", err, "job", id)
	if err != nil {
		jobError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func jobError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, gateway.ErrJobNotFound):
		httpError(w, http.StatusNotFound, err.Error())
	default:
		httpError(w, http.StatusBadRequest, err.Error())
	}
}

// jobsCollector exports the number of jobs per status at scrape time.
type jobsCollector struct{ j *gateway.Jobs }

var descJobs = prometheus.NewDesc("phoneborg_superborg_jobs", "Super Borg jobs by status (ADR-021).", []string{"status"}, nil)

func (c *jobsCollector) Describe(ch chan<- *prometheus.Desc) { ch <- descJobs }

func (c *jobsCollector) Collect(ch chan<- prometheus.Metric) {
	n := map[string]int{}
	for _, st := range []string{gateway.JobQueued, gateway.JobRunning, gateway.JobWaiting, gateway.JobDone, gateway.JobFailed, gateway.JobCancelled} {
		n[st] = 0
	}
	for _, j := range c.j.List() {
		n[j.Status]++
	}
	for st, v := range n {
		ch <- prometheus.MustNewConstMetric(descJobs, prometheus.GaugeValue, float64(v), st)
	}
}
