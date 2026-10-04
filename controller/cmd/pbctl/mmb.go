package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/dzaczek/phoneborg/controller"
)

// mmbCmd drives the multi-model benchmark (ADR-028):
//
//	mmb
//	mmb run [nodes=a,b] [pool=P] [models=x,y] [parallel]
//	mmb show|cancel|rm <id>
func mmbCmd(c *client, o *out, args []string) error {
	switch {
	case len(args) == 0:
		raw, err := c.do(http.MethodGet, "/admin/mmb", nil)
		if err != nil || o.json {
			return o.done(raw, err, "")
		}
		var l controller.MMBRuns
		if err := json.Unmarshal(raw, &l); err != nil {
			return err
		}
		if len(l.Runs) == 0 {
			fmt.Fprintln(o.w, "no benchmark runs")
			return nil
		}
		rows := [][]string{}
		for _, r := range l.Runs {
			done := 0
			for _, x := range r.Results {
				if x.Status == controller.MMBDone {
					done++
				}
			}
			mode := "one by one"
			if r.Parallel {
				mode = "parallel"
			}
			rows = append(rows, []string{r.ID, r.Status, mode, strings.Join(r.Nodes, ","), fmt.Sprintf("%d/%d", done, len(r.Results)),
				r.Created.Local().Format("2006-01-02 15:04")})
		}
		o.table("ID\tSTATUS\tMODE\tNODES\tDONE\tSTARTED", rows)
		return nil
	case args[0] == "run":
		var req controller.MMBRequest
		for _, a := range args[1:] {
			k, v, _ := strings.Cut(a, "=")
			switch k {
			case "nodes":
				req.Nodes = list(v)
			case "pool":
				req.Pool = v
			case "models":
				req.Models = list(v)
			case "parallel":
				req.Parallel = true
			default:
				return fmt.Errorf("unknown mmb setting %q (nodes, pool, models, parallel)", a)
			}
		}
		raw, err := c.do(http.MethodPost, "/admin/mmb", req)
		if err != nil || o.json {
			return o.done(raw, err, "")
		}
		var r controller.MMBRun
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		fmt.Fprintf(o.w, "benchmark %s started: %d model/node pairs on %s\nfollow it with: pbctl mmb show %s\n",
			r.ID, len(r.Results), strings.Join(r.Nodes, ","), r.ID)
		return nil
	case len(args) == 2 && args[0] == "show":
		raw, err := c.do(http.MethodGet, "/admin/mmb/"+pathEscape(args[1]), nil)
		if err != nil || o.json {
			return o.done(raw, err, "")
		}
		var r controller.MMBRun
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		fmt.Fprintf(o.w, "%s  [%s]  %s\n\n", r.ID, r.Status, strings.Join(r.Nodes, ","))
		rows := [][]string{}
		for _, x := range r.Results {
			node := x.NodeID
			if x.Alias != "" {
				node = x.Alias
			}
			rows = append(rows, []string{node, x.ModelID, x.Status, fnum(x.LoadSeconds, "%.0fs"),
				probeCell(x.Cold, "ttft"), probeCell(x.Cold, "pp"), probeCell(x.Cold, "tg"),
				probeCell(x.Warm, "ttft"), probeCell(x.Short, "total"), x.Error})
		}
		o.table("NODE\tMODEL\tSTATUS\tLOAD\tCOLD TTFT\tPROMPT T/S\tGEN T/S\tWARM TTFT\tSHORT\tNOTE", rows)
		return nil
	case len(args) == 2 && args[0] == "cancel":
		raw, err := c.do(http.MethodPost, "/admin/mmb/"+pathEscape(args[1])+"/cancel", nil)
		return o.done(raw, err, "benchmark "+args[1]+" cancelled; its phones get their models back")
	case len(args) == 2 && args[0] == "rm":
		raw, err := c.do(http.MethodDelete, "/admin/mmb/"+pathEscape(args[1]), nil)
		return o.done(raw, err, "benchmark "+args[1]+" removed")
	}
	return errUsage
}

func fnum(v float64, format string) string {
	if v <= 0 {
		return "-"
	}
	return fmt.Sprintf(format, v)
}

// probeCell renders one measurement of a probe.
func probeCell(p *controller.MMBProbe, what string) string {
	if p == nil {
		return "-"
	}
	switch what {
	case "ttft":
		return fnum(p.TTFTMs/1000, "%.1fs")
	case "total":
		return fnum(p.TotalMs/1000, "%.1fs")
	case "pp":
		return fnum(p.PromptTPS, "%.1f")
	case "tg":
		return fnum(p.GenTPS, "%.1f")
	}
	return "-"
}
