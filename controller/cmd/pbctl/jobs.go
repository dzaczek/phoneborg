package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/dzaczek/phoneborg/controller"
	"github.com/dzaczek/phoneborg/controller/gateway"
)

// jobsCmd manages Super Borg jobs (ADR-021):
//
//	jobs
//	jobs new [title=T] <goal...>
//	jobs show|cancel|rm|result <id>
//	jobs say <id> <text...>
//	jobs doc <id> <name>
func jobsCmd(c *client, o *out, args []string) error {
	if len(args) == 0 {
		raw, err := c.do(http.MethodGet, "/admin/jobs", nil)
		if err != nil {
			return err
		}
		if o.json {
			return o.raw(raw)
		}
		var l controller.JobList
		if err := json.Unmarshal(raw, &l); err != nil {
			return err
		}
		if len(l.Jobs) == 0 {
			fmt.Fprintln(o.w, "no jobs")
			return nil
		}
		rows := [][]string{}
		for _, j := range l.Jobs {
			rows = append(rows, []string{j.ID, j.Status, strconv.Itoa(j.Steps), fmt.Sprintf("%d/%d", j.TasksDone, j.Tasks), strconv.Itoa(j.Docs), j.Title})
		}
		o.table("ID\tSTATUS\tSTEPS\tTASKS\tDOCS\tTITLE", rows)
		return nil
	}
	sub, rest := args[0], args[1:]
	switch {
	case sub == "new" && len(rest) > 0:
		var req controller.JobCreate
		if t, ok := strings.CutPrefix(rest[0], "title="); ok {
			req.Title, rest = t, rest[1:]
		}
		req.Goal = strings.Join(rest, " ")
		raw, err := c.do(http.MethodPost, "/admin/jobs", req)
		if err != nil || o.json {
			return o.done(raw, err, "")
		}
		var j gateway.JobSummary
		if err := json.Unmarshal(raw, &j); err != nil {
			return err
		}
		fmt.Fprintf(o.w, "job %s queued: %s\nfollow it with: pbctl jobs show %s\n", j.ID, j.Title, j.ID)
		return nil
	case sub == "show" && len(rest) == 1:
		raw, err := c.do(http.MethodGet, "/admin/jobs/"+pathEscape(rest[0]), nil)
		if err != nil {
			return err
		}
		return showJob(o, raw)
	case sub == "say" && len(rest) > 1:
		raw, err := c.do(http.MethodPost, "/admin/jobs/"+pathEscape(rest[0])+"/messages", controller.JobMessage{Text: strings.Join(rest[1:], " ")})
		return o.done(raw, err, "message sent to job "+rest[0])
	case sub == "cancel" && len(rest) == 1:
		raw, err := c.do(http.MethodPost, "/admin/jobs/"+pathEscape(rest[0])+"/cancel", nil)
		return o.done(raw, err, "job "+rest[0]+" cancelled (send a message to resume it)")
	case sub == "rm" && len(rest) == 1:
		raw, err := c.do(http.MethodDelete, "/admin/jobs/"+pathEscape(rest[0]), nil)
		return o.done(raw, err, "job "+rest[0]+" removed")
	case sub == "result" && len(rest) == 1:
		raw, err := c.do(http.MethodGet, "/admin/jobs/"+pathEscape(rest[0])+"/result", nil)
		if err != nil {
			return err
		}
		_, err = o.w.Write(raw)
		return err
	case sub == "doc" && len(rest) == 2:
		raw, err := c.do(http.MethodGet, "/admin/jobs/"+pathEscape(rest[0])+"/docs/"+pathEscape(rest[1]), nil)
		if err != nil || o.json {
			return o.done(raw, err, "")
		}
		var d gateway.JobDoc
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		fmt.Fprintln(o.w, d.Text)
		return nil
	}
	return errUsage
}

func showJob(o *out, raw []byte) error {
	if o.json {
		return o.raw(raw)
	}
	var j gateway.Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return err
	}
	fmt.Fprintf(o.w, "%s  %s  [%s]  %d steps\n", j.ID, j.Title, j.Status, j.Steps)
	if j.Question != "" {
		fmt.Fprintf(o.w, "waiting: %s\n", j.Question)
	}
	if j.Error != "" {
		fmt.Fprintf(o.w, "error: %s\n", j.Error)
	}
	rows := [][]string{}
	for _, t := range j.Tasks {
		rows = append(rows, []string{strconv.Itoa(t.ID), t.Status, t.Doc, t.Title})
	}
	fmt.Fprintln(o.w)
	o.table("TASK\tSTATUS\tDOC\tTITLE", rows)
	rows = [][]string{}
	for _, d := range j.Docs {
		rows = append(rows, []string{d.Name, d.Author, strings.Join(strings.Fields(d.Text), " ")})
	}
	fmt.Fprintln(o.w)
	o.table("DOC\tAUTHOR\tSTART", rows)
	fmt.Fprintln(o.w)
	ev := j.Events
	if len(ev) > 15 {
		ev = ev[len(ev)-15:]
	}
	for _, e := range ev {
		fmt.Fprintf(o.w, "%s  %-8s %s\n", e.Time.Local().Format("15:04:05"), e.Kind, strings.Join(strings.Fields(e.Text), " "))
	}
	return nil
}
