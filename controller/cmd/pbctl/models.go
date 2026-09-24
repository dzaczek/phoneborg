package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/dzaczek/phoneborg/controller"
	"github.com/dzaczek/phoneborg/controller/models"
)

// Model catalog and placement commands (ADR-011).

func modelsCmd(c *client, o *out, rest []string) error {
	if len(rest) == 0 {
		return listModels(c, o)
	}
	sub, args := rest[0], rest[1:]
	switch {
	case sub == "add" && len(args) >= 1:
		req, err := parseModelAdd(args)
		if err != nil {
			return err
		}
		raw, err := c.do(http.MethodPost, "/admin/models", req)
		if err != nil {
			return err
		}
		var m models.Model
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		return o.done(raw, nil, fmt.Sprintf("model %s is downloading; follow it with pbctl models", m.ID))
	case sub == "rm" && len(args) == 1:
		raw, err := c.do(http.MethodDelete, "/admin/models/"+pathEscape(args[0]), nil)
		return o.done(raw, err, fmt.Sprintf("model %s removed", args[0]))
	case sub == "tag" && len(args) == 2:
		raw, err := c.do(http.MethodPatch, "/admin/models/"+pathEscape(args[0]), models.Patch{Tags: list(args[1])})
		return o.done(raw, err, fmt.Sprintf("model %s tagged %s", args[0], args[1]))
	case sub == "recommend" && len(args) >= 2:
		p, err := parseRecommend(args[1:])
		if err != nil {
			return err
		}
		raw, err := c.do(http.MethodPatch, "/admin/models/"+pathEscape(args[0]), p)
		return o.done(raw, err, fmt.Sprintf("model %s recommended", args[0]))
	case sub == "default" && len(args) == 1 && args[0] == "none":
		pl, err := getPlacement(c)
		if err != nil {
			return err
		}
		raw, err := c.do(http.MethodPut, "/admin/placement", models.Spec{Policies: pl.Policies})
		return o.done(raw, err, "no default model")
	case sub == "default" && len(args) == 1:
		on := true
		raw, err := c.do(http.MethodPatch, "/admin/models/"+pathEscape(args[0]), models.Patch{Default: &on})
		return o.done(raw, err, fmt.Sprintf("default model is %s", args[0]))
	}
	return errUsage
}

// list splits "a,b" into ["a","b"]; "" and "-" give an empty list.
func list(s string) []string {
	out := []string{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" && v != "-" {
			out = append(out, v)
		}
	}
	return out
}

// parseModelAdd parses "<source> [-id x] [-name n] [-tag a,b] [-recommend s,m]".
func parseModelAdd(args []string) (models.AddRequest, error) {
	req := models.AddRequest{Source: args[0]}
	for i := 1; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return req, fmt.Errorf("%s needs a value", args[i])
		}
		v := args[i+1]
		switch strings.TrimLeft(args[i], "-") {
		case "id":
			req.ID = v
		case "name":
			req.Name = v
		case "tag", "tags":
			req.Tags = list(v)
		case "recommend":
			req.RecommendedClasses = list(v)
		default:
			return req, fmt.Errorf("unknown option %q (-id, -name, -tag, -recommend)", args[i])
		}
	}
	return req, nil
}

// parseRecommend parses "classes=s,m tiers=t2,t3", or the older positional
// "s,m" (classes only, kept working).
func parseRecommend(args []string) (models.Patch, error) {
	var p models.Patch
	if len(args) == 1 && !strings.Contains(args[0], "=") {
		p.RecommendedClasses = list(args[0])
		return p, nil
	}
	for _, kv := range args {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return p, fmt.Errorf("want key=value, got %q", kv)
		}
		switch k {
		case "classes":
			p.RecommendedClasses = list(v)
		case "tiers":
			p.RecommendedTiers = list(v)
		default:
			return p, fmt.Errorf("unknown setting %q (classes, tiers)", k)
		}
	}
	return p, nil
}

func listModels(c *client, o *out) error {
	raw, err := c.do(http.MethodGet, "/admin/models", nil)
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	var ms controller.ModelList
	if err := json.Unmarshal(raw, &ms); err != nil {
		return err
	}
	if len(ms.Models) == 0 {
		fmt.Fprintln(o.w, "no models in the catalog; add one with pbctl models add <source>")
		return nil
	}
	var rows [][]string
	var errs []string
	for _, m := range ms.Models {
		id, status, size := m.ID, m.Status, "-"
		if m.Default {
			id += " (default)"
		}
		switch m.Status {
		case models.StatusDownloading:
			status = fmt.Sprintf("downloading %.0f%%", m.Progress*100)
		case models.StatusError:
			errs = append(errs, fmt.Sprintf("%s: %s", m.ID, m.Error))
		}
		if m.SizeBytes > 0 {
			size = gib(m.SizeBytes)
		}
		rows = append(rows, []string{id, dash(m.Params), dash(m.Quant), size, status, dash(strings.Join(m.FitsClasses, ",")),
			dash(strings.Join(m.Tags, ",")), dash(strings.Join(m.RecommendedClasses, ",")), strconv.Itoa(m.NodesServing)})
	}
	o.table("MODEL\tPARAMS\tQUANT\tSIZE\tSTATUS\tFITS (16k)\tTAGS\tRECOMMENDED\tSERVING", rows)
	for _, e := range errs {
		fmt.Fprintln(o.w, "error:", e)
	}
	return nil
}

func classes(c *client, o *out) error {
	raw, err := c.do(http.MethodGet, "/admin/device-classes", nil)
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	var dc controller.DeviceClasses
	if err := json.Unmarshal(raw, &dc); err != nil {
		return err
	}
	var rows [][]string
	for _, cl := range dc.Classes {
		rows = append(rows, []string{cl.ID, cl.Label, strconv.Itoa(cl.Nodes), dash(strings.Join(cl.RecommendedModels, ","))})
	}
	o.table("CLASS\tRAM\tNODES\tRECOMMENDED MODELS", rows)
	var tierRows [][]string
	for _, t := range dc.PerfTiers {
		tierRows = append(tierRows, []string{t.ID, t.Label, strconv.Itoa(t.Nodes), dash(strings.Join(t.RecommendedModels, ","))})
	}
	fmt.Fprintln(o.w)
	o.table("TIER\tGB/S\tNODES\tRECOMMENDED MODELS", tierRows)
	return nil
}

func getPlacement(c *client) (controller.Placement, error) {
	var pl controller.Placement
	raw, err := c.do(http.MethodGet, "/admin/placement", nil)
	if err == nil {
		err = json.Unmarshal(raw, &pl)
	}
	return pl, err
}

func placementCmd(c *client, o *out, rest []string) error {
	if len(rest) == 0 {
		raw, err := c.do(http.MethodGet, "/admin/placement", nil)
		return showPlacement(o, raw, err, "")
	}
	sub, args := rest[0], rest[1:]
	path, title := "/admin/placement", ""
	if sub == "preview" {
		path, title = "/admin/placement/preview", "preview, nothing applied"
		if len(args) == 0 { // preview the current placement
			pl, err := getPlacement(c)
			if err != nil {
				return err
			}
			raw, err := c.do(http.MethodPost, path, models.Spec{Policies: pl.Policies, DefaultModel: pl.DefaultModel})
			return showPlacement(o, raw, err, title)
		}
		sub, args = args[0], args[1:]
		if sub != "set" && sub != "unset" { // "preview <model> k=v" = "preview set <model> k=v"
			sub, args = "set", rest[1:]
		}
	}
	if len(args) == 0 || (sub != "set" && sub != "unset") || (sub == "unset" && len(args) != 1) {
		return errUsage
	}
	pl, err := getPlacement(c)
	if err != nil {
		return err
	}
	model := args[0]
	idx := slices.IndexFunc(pl.Policies, func(p models.Policy) bool { return p.ModelID == model })
	if sub == "unset" {
		if idx < 0 {
			return fmt.Errorf("no policy for model %s", model)
		}
		pl.Policies = slices.Delete(pl.Policies, idx, idx+1)
	} else {
		p, err := parsePolicy(model, args[1:])
		if err != nil {
			return err
		}
		if idx >= 0 {
			pl.Policies[idx] = p
		} else {
			pl.Policies = append(pl.Policies, p)
		}
	}
	method := http.MethodPut
	if title != "" {
		method = http.MethodPost
	}
	raw, err := c.do(method, path, models.Spec{Policies: pl.Policies, DefaultModel: pl.DefaultModel})
	return showPlacement(o, raw, err, title)
}

// parsePolicy parses "pin=a,b" | "replicas=2" | "percent=25", plus an
// optional "classes=s,m".
func parsePolicy(model string, kvs []string) (models.Policy, error) {
	p := models.Policy{ModelID: model}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			return p, fmt.Errorf("want key=value, got %q", kv)
		}
		mode := p.Mode
		var err error
		switch k {
		case "pin":
			mode, p.Nodes = models.ModePin, list(v)
		case "replicas":
			mode = models.ModeReplicas
			p.Replicas, err = strconv.Atoi(v)
		case "percent":
			mode = models.ModePercent
			p.Percent, err = strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64)
		case "classes":
			p.Classes = list(v)
		case "min_tok_s":
			p.MinTokS, err = strconv.ParseFloat(v, 64)
		default:
			return p, fmt.Errorf("unknown setting %q (pin, replicas, percent, classes, min_tok_s)", k)
		}
		if err != nil {
			return p, fmt.Errorf("%s: %w", k, err)
		}
		if p.Mode != "" && mode != p.Mode {
			return p, fmt.Errorf("choose one of pin, replicas or percent")
		}
		p.Mode = mode
	}
	if p.Mode == "" {
		return p, fmt.Errorf("want pin=<node,...>, replicas=<n> or percent=<p>")
	}
	return p, nil
}

func showPlacement(o *out, raw []byte, err error, title string) error {
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	var pl controller.Placement
	if err := json.Unmarshal(raw, &pl); err != nil {
		return err
	}
	if title != "" {
		fmt.Fprintf(o.w, "(%s)\n", title)
	}
	fmt.Fprintf(o.w, "default model: %s\n\n", dash(pl.DefaultModel))
	if len(pl.Policies) == 0 {
		fmt.Fprintln(o.w, "no policies")
	} else {
		var rows [][]string
		for _, p := range pl.Policies {
			target := ""
			switch p.Mode {
			case models.ModePin:
				target = strings.Join(p.Nodes, ",")
			case models.ModeReplicas:
				target = strconv.Itoa(p.Replicas)
			case models.ModePercent:
				target = strconv.FormatFloat(p.Percent, 'f', -1, 64) + "%"
			}
			minTokS := "-"
			if p.MinTokS > 0 {
				minTokS = strconv.FormatFloat(p.MinTokS, 'f', -1, 64)
			}
			rows = append(rows, []string{p.ModelID, p.Mode, target, dash(strings.Join(p.Classes, ",")), minTokS})
		}
		o.table("MODEL\tMODE\tTARGET\tCLASSES\tMIN TOK/S", rows)
	}
	fmt.Fprintln(o.w)
	state := map[string]controller.PlacementNode{}
	for _, n := range pl.Nodes {
		state[n.NodeID] = n
	}
	if len(pl.Plan) == 0 {
		fmt.Fprintln(o.w, "no nodes")
	} else {
		var rows [][]string
		for _, a := range pl.Plan {
			n := state[a.NodeID]
			st := n.State
			if n.State == "downloading" {
				st = fmt.Sprintf("downloading %.0f%%", n.Progress*100)
			}
			if n.Drained {
				st += " DRAINED"
			}
			fits, est := "yes", "-"
			if !a.Fits {
				fits = "NO"
			}
			if a.EstRAMBytes > 0 {
				est = gib(a.EstRAMBytes)
			}
			budget := "-"
			if n.BudgetBytes > 0 {
				budget = gib(n.BudgetBytes)
			}
			pred := "-"
			if a.PredGenTPS > 0 {
				pred = fmt.Sprintf("%.1f", a.PredGenTPS)
			}
			rows = append(rows, []string{a.NodeID, n.Class + "/" + dash(n.PerfTier), gib(int64(n.RAMTotalBytes)), budget, dash(n.CurrentModel), st,
				dash(a.ModelID), a.Reason, est, fits, pred})
		}
		o.table("NODE\tCLASS\tRAM\tBUDGET\tCURRENT\tSTATE\tPLANNED\tREASON\tEST RAM\tFITS\tPRED TOK/S", rows)
	}
	for _, n := range pl.Nodes {
		if n.Error != "" {
			fmt.Fprintf(o.w, "node %s: %s\n", n.NodeID, n.Error)
		}
	}
	for _, w := range pl.Warnings {
		fmt.Fprintln(o.w, "warning:", w)
	}
	return nil
}

func gib(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
