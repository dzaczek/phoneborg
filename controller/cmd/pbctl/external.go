package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dzaczek/phoneborg/controller"
)

// External engine nodes (ADR-016).

// selfTestTimeout bounds "external selftest": every model generates up to
// 32 tokens, and a server may first have to load the model.
const selfTestTimeout = 10 * time.Minute

func externalCmd(c *client, o *out, rest []string) error {
	switch {
	case len(rest) == 0:
		return listExternal(c, o)
	case rest[0] == "add" && len(rest) >= 3:
		spec, err := parseExternalAdd(rest[2], rest[3:])
		if err != nil {
			return err
		}
		raw, err := c.do(http.MethodPut, "/admin/external/"+pathEscape(rest[1]), spec)
		if err != nil {
			return err
		}
		var x controller.External
		if err := json.Unmarshal(raw, &x); err != nil {
			return err
		}
		msg := fmt.Sprintf("external node %s saved (node/%s, node id %s): %s, %d model(s)", x.Name, x.Name, x.NodeID, x.State, len(x.DiscoveredModels))
		if x.LastError != "" {
			msg += "\nlast error: " + x.LastError
		}
		return o.done(raw, nil, msg)
	case rest[0] == "rm" && len(rest) == 2:
		raw, err := c.do(http.MethodDelete, "/admin/external/"+pathEscape(rest[1]), nil)
		return o.done(raw, err, fmt.Sprintf("external node %s removed", rest[1]))
	case rest[0] == "selftest" && len(rest) == 2:
		c.http.Timeout = selfTestTimeout
		raw, err := c.do(http.MethodPost, "/admin/external/"+pathEscape(rest[1])+"/selftest", nil)
		if err != nil {
			return err
		}
		if o.json {
			return o.raw(raw)
		}
		var x controller.External
		if err := json.Unmarshal(raw, &x); err != nil {
			return err
		}
		printSelfTest(o, x)
		return nil
	}
	return errUsage
}

// parseExternalAdd builds a PUT body from "<url> [key-file=path]
// [models=a,b] [concurrency=N] [ctx=N] [speed=N]".
func parseExternalAdd(url string, kvs []string) (controller.ExternalSpec, error) {
	spec := controller.ExternalSpec{URL: url}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			return spec, fmt.Errorf("want key=value, got %q", kv)
		}
		var err error
		switch k {
		case "key-file":
			var key string
			if key, err = readKeyFile(v); err == nil {
				spec.APIKey = &key
			}
		case "models":
			spec.Models = list(v)
		case "concurrency":
			spec.MaxConcurrency, err = strconv.Atoi(v)
		case "ctx":
			spec.CtxSize, err = strconv.Atoi(v)
		case "speed":
			spec.SpeedTPS, err = strconv.ParseFloat(v, 64)
		default:
			return spec, fmt.Errorf("unknown external setting %q (key-file, models, concurrency, ctx, speed)", k)
		}
		if err != nil {
			return spec, fmt.Errorf("%s: %w", k, err)
		}
	}
	return spec, nil
}

// readKeyFile returns the first line of path that is not blank or a #
// comment. "-" as the path removes the key.
func readKeyFile(path string) (string, error) {
	if path == "-" {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			return line, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New(path + ": no API key")
}

func getExternal(c *client) ([]byte, []controller.External, error) {
	raw, err := c.do(http.MethodGet, "/admin/external", nil)
	if err != nil {
		return nil, nil, err
	}
	var xs controller.Externals
	return raw, xs.External, json.Unmarshal(raw, &xs)
}

func listExternal(c *client, o *out) error {
	raw, xs, err := getExternal(c)
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	if len(xs) == 0 {
		fmt.Fprintln(o.w, "no external nodes")
		return nil
	}
	var rows [][]string
	for _, x := range xs {
		key := "-"
		if x.HasAPIKey {
			key = "yes"
		}
		ctx := "-"
		if x.CtxSize > 0 {
			ctx = strconv.Itoa(x.CtxSize)
		}
		rows = append(rows, []string{x.Name, x.URL, externalState(x), orAny(x.Models), dash(strings.Join(x.DiscoveredModels, ",")),
			fmt.Sprintf("%d/%d", x.Inflight, x.MaxConcurrency), ctx, externalTPS(x), key, ago(x.LastCheck), dash(x.LastError)})
	}
	o.table("NAME\tURL\tSTATE\tALLOW\tMODELS\tINFLIGHT\tCTX\tTOK/S\tKEY\tLAST CHECK\tLAST ERROR", rows)
	return nil
}

func externalState(x controller.External) string {
	if x.Drained {
		return x.State + " DRAINED"
	}
	return x.State
}

// externalTPS is the operator's speed hint, else the fastest self-test.
func externalTPS(x controller.External) string {
	if x.SpeedTPS > 0 {
		return strconv.FormatFloat(x.SpeedTPS, 'f', 1, 64) + " (hint)"
	}
	best := 0.0
	for _, sp := range x.Measured {
		best = max(best, sp.GenTPS)
	}
	if best == 0 {
		return "-"
	}
	return strconv.FormatFloat(best, 'f', 1, 64)
}

func printSelfTest(o *out, x controller.External) {
	models := make([]string, 0, len(x.Measured))
	for m := range x.Measured {
		models = append(models, m)
	}
	sort.Strings(models)
	if len(models) == 0 {
		fmt.Fprintf(o.w, "%s: nothing measured (%s, %d model(s))\n", x.Name, x.State, len(x.DiscoveredModels))
		return
	}
	var rows [][]string
	for _, m := range models {
		sp := x.Measured[m]
		gen, prompt := "-", "-"
		if sp.GenTPS > 0 {
			gen = strconv.FormatFloat(sp.GenTPS, 'f', 1, 64)
		}
		if sp.PromptTPS > 0 {
			prompt = strconv.FormatFloat(sp.PromptTPS, 'f', 1, 64)
		}
		rows = append(rows, []string{m, gen, prompt, dash(sp.Error)})
	}
	o.table("MODEL\tGEN TOK/S\tPROMPT TOK/S\tERROR", rows)
}

// externalNodeRows renders external nodes as rows of the "pbctl nodes"
// table.
func externalNodeRows(xs []controller.External) [][]string {
	var rows [][]string
	for _, x := range xs {
		model, runtime := "-", "-"
		if n := len(x.DiscoveredModels); n > 0 {
			model = x.DiscoveredModels[0]
			if n > 1 {
				model += fmt.Sprintf(" (+%d)", n-1)
			}
		}
		if x.State == controller.ExternalActive {
			runtime = "ready"
		}
		drained := "-"
		if x.Drained {
			drained = "DRAINED"
		}
		rows = append(rows, []string{x.Name + " (" + x.NodeID + ")", "external", "-", x.State, drained, x.URL, model, runtime, "-",
			strings.TrimSuffix(externalTPS(x), " (hint)"), fmt.Sprintf("%d/%d", x.Inflight, x.MaxConcurrency), strconv.Itoa(x.PinnedSessions), ago(x.LastCheck)})
	}
	return rows
}
