// Command pbctl manages a PhoneBorg cluster through the controller's admin
// API: nodes, draining, API keys, usage statistics and gateway settings.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dzaczek/phoneborg/controller"
)

const usageText = `usage: pbctl [-json] [-url URL] [-token-file FILE] <command> [args]

commands:
  nodes                       list nodes (state, drain, runtime, load)
  drain <id>                  stop sending new requests to a node
  undrain <id>                put a drained node back into rotation
  forget <id>                 remove a node (it re-registers if alive)
  keys                        list API keys with usage
  keys create <name>          create an API key (printed once)
  keys revoke <name>          revoke all keys of <name>
  stats                       usage statistics and cluster summary
  gateway                     show gateway settings
  gateway set k=v ...         change settings: policy=affinity|least_inflight
                              spill=<n> timeout=<duration> auth=keys
                              thermal_limit=<celsius, 0 disables>
  models                      list served models (/v1/models)

environment:
  PHONEBORG_URL          controller URL (default http://127.0.0.1:18080)
  PHONEBORG_ADMIN_TOKEN  admin token (or -token-file)
  PHONEBORG_API_KEY      API key for "models" when the gateway enforces keys
`

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pbctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usageText) }
	asJSON := fs.Bool("json", false, "print raw JSON")
	baseURL := fs.String("url", "", "controller URL (overrides PHONEBORG_URL)")
	tokenFile := fs.String("token-file", "", "file with the admin token (overrides PHONEBORG_ADMIN_TOKEN)")
	// Accept -json anywhere, e.g. "pbctl nodes -json".
	var rest []string
	for _, a := range args {
		if a == "-json" || a == "--json" {
			*asJSON = true
		} else {
			rest = append(rest, a)
		}
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	c := &client{base: strings.TrimRight(firstNonEmpty(*baseURL, getenv("PHONEBORG_URL"), "http://127.0.0.1:18080"), "/"),
		apiKey: getenv("PHONEBORG_API_KEY"), http: &http.Client{Timeout: 30 * time.Second}}
	c.token = getenv("PHONEBORG_ADMIN_TOKEN")
	if *tokenFile != "" {
		tok, err := controller.LoadAdminToken(*tokenFile)
		if err != nil {
			fmt.Fprintln(stderr, "pbctl:", err)
			return 1
		}
		c.token = tok
	}
	o := &out{w: stdout, json: *asJSON}
	if err := dispatch(c, o, fs.Args()); err != nil {
		if errors.Is(err, errUsage) {
			fs.Usage()
			return 2
		}
		fmt.Fprintln(stderr, "pbctl:", err)
		return 1
	}
	return 0
}

var errUsage = errors.New("usage")

func dispatch(c *client, o *out, args []string) error {
	cmd, rest := args[0], args[1:]
	one := func() (string, error) {
		if len(rest) != 1 {
			return "", errUsage
		}
		return rest[0], nil
	}
	switch {
	case cmd == "nodes" && len(rest) == 0:
		return nodes(c, o)
	case cmd == "drain" || cmd == "undrain":
		id, err := one()
		if err != nil {
			return err
		}
		raw, err := c.do(http.MethodPost, "/admin/nodes/"+pathEscape(id)+"/"+cmd, nil)
		return o.done(raw, err, fmt.Sprintf("node %s %sed", id, cmd))
	case cmd == "forget":
		id, err := one()
		if err != nil {
			return err
		}
		raw, err := c.do(http.MethodDelete, "/admin/nodes/"+pathEscape(id), nil)
		return o.done(raw, err, fmt.Sprintf("node %s forgotten (it re-registers if still alive)", id))
	case cmd == "keys" && len(rest) == 0:
		return keys(c, o)
	case cmd == "keys" && len(rest) == 2 && rest[0] == "create":
		return createKey(c, o, rest[1])
	case cmd == "keys" && len(rest) == 2 && rest[0] == "revoke":
		raw, err := c.do(http.MethodDelete, "/admin/keys/"+pathEscape(rest[1]), nil)
		return o.done(raw, err, fmt.Sprintf("keys of %s revoked", rest[1]))
	case cmd == "stats" && len(rest) == 0:
		return stats(c, o)
	case cmd == "gateway" && len(rest) == 0:
		raw, err := c.do(http.MethodGet, "/admin/gateway", nil)
		return showGateway(o, raw, err)
	case cmd == "gateway" && len(rest) > 1 && rest[0] == "set":
		u, err := parseGatewaySet(rest[1:])
		if err != nil {
			return err
		}
		raw, err := c.do(http.MethodPut, "/admin/gateway", u)
		return showGateway(o, raw, err)
	case cmd == "models" && len(rest) == 0:
		return models(c, o)
	}
	return errUsage
}

// parseGatewaySet turns "policy=affinity spill=3 timeout=600s auth=keys"
// into an update.
func parseGatewaySet(kvs []string) (controller.GatewayUpdate, error) {
	var u controller.GatewayUpdate
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			return u, fmt.Errorf("want key=value, got %q", kv)
		}
		switch k {
		case "policy":
			u.Policy = &v
		case "spill":
			n, err := strconv.Atoi(v)
			if err != nil {
				return u, fmt.Errorf("spill: %w", err)
			}
			u.AffinitySpill = &n
		case "timeout":
			u.UpstreamTimeout = &v
		case "auth":
			u.AuthMode = &v
		case "thermal_limit":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return u, fmt.Errorf("thermal_limit: %w", err)
			}
			u.ThermalLimitC = &f
		default:
			return u, fmt.Errorf("unknown setting %q (policy, spill, timeout, auth, thermal_limit)", k)
		}
	}
	return u, nil
}

type client struct {
	base, token, apiKey string
	http                *http.Client
}

// do calls the admin API and returns the response body. Non-2xx responses
// become errors carrying the server's message.
func (c *client) do(method, path string, body any) ([]byte, error) {
	if c.token == "" {
		return nil, errors.New("no admin token: set PHONEBORG_ADMIN_TOKEN or use -token-file")
	}
	return c.request(method, path, body, c.token)
}

func (c *client) request(method, path string, body any, bearer string) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, errorMessage(raw))
	}
	return raw, nil
}

// errorMessage extracts {"error":"..."} or OpenAI's {"error":{"message":...}}.
func errorMessage(raw []byte) string {
	var v struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &v) == nil && len(v.Error) > 0 {
		var s string
		if json.Unmarshal(v.Error, &s) == nil {
			return s
		}
		var o struct{ Message string }
		if json.Unmarshal(v.Error, &o) == nil && o.Message != "" {
			return o.Message
		}
	}
	return strings.TrimSpace(string(raw))
}

type out struct {
	w    io.Writer
	json bool
}

// raw prints a JSON body indented.
func (o *out) raw(b []byte) error {
	if len(bytes.TrimSpace(b)) == 0 {
		b = []byte("{}")
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, b, "", "  "); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err := o.w.Write(buf.Bytes())
	return err
}

// done reports the outcome of a command that changes state.
func (o *out) done(raw []byte, err error, msg string) error {
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	_, err = fmt.Fprintln(o.w, msg)
	return err
}

func (o *out) table(header string, rows [][]string) {
	tw := tabwriter.NewWriter(o.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

func nodes(c *client, o *out) error {
	raw, err := c.do(http.MethodGet, "/admin/nodes", nil)
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	var ns []controller.AdminNode
	if err := json.Unmarshal(raw, &ns); err != nil {
		return err
	}
	if len(ns) == 0 {
		fmt.Fprintln(o.w, "no nodes")
		return nil
	}
	var rows [][]string
	for _, n := range ns {
		model, runtime, build, toks := "-", "-", "-", "-"
		if hb := n.LastHeartbeat; hb != nil && hb.Runtime != nil {
			rt := hb.Runtime
			model, runtime = rt.Model, map[bool]string{true: "ready", false: "loading"}[rt.Ready]
			build = strings.TrimPrefix(rt.Engine, "llama.cpp/")
			if rt.Threads > 0 {
				build += fmt.Sprintf(" %dt", rt.Threads)
			}
			if rt.CtxSize > 0 {
				build += fmt.Sprintf(" %dk", rt.CtxSize/1024)
			}
			if rt.GenTPS > 0 {
				toks = fmt.Sprintf("%.1f", rt.GenTPS)
			}
		}
		drained := "-"
		if n.Drained {
			drained = "DRAINED"
		}
		state := string(n.State)
		if n.Hot {
			state += " HOT"
		}
		rows = append(rows, []string{n.ID, state, drained, strings.TrimSpace(n.Inventory.Manufacturer + " " + n.Inventory.Model),
			model, runtime, build, toks, strconv.Itoa(n.Inflight), strconv.Itoa(n.PinnedSessions), ago(n.LastSeen)})
	}
	o.table("NODE\tSTATE\tDRAIN\tDEVICE\tMODEL\tRUNTIME\tBUILD\tTOK/S\tINFLIGHT\tPINNED\tLAST SEEN", rows)
	return nil
}

func keys(c *client, o *out) error {
	raw, err := c.do(http.MethodGet, "/admin/keys", nil)
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	var ks controller.AdminKeys
	if err := json.Unmarshal(raw, &ks); err != nil {
		return err
	}
	persisted := "in memory only (no -api-keys-file)"
	if ks.Persisted {
		persisted = "persisted to -api-keys-file"
	}
	fmt.Fprintf(o.w, "auth mode: %s, keys %s\n", ks.AuthMode, persisted)
	if len(ks.Keys) == 0 {
		fmt.Fprintln(o.w, "no keys")
		return nil
	}
	var rows [][]string
	for _, k := range ks.Keys {
		created := "-"
		if !k.Created.IsZero() {
			created = k.Created.Local().Format("2006-01-02 15:04")
		}
		u := k.Usage
		rows = append(rows, []string{k.Name, strconv.Itoa(k.Keys), created, num(u.Requests), num(u.Errors),
			num(u.PromptTokens), num(u.CachedPromptTokens), num(u.CompletionTokens), ago(u.LastUsed)})
	}
	o.table("NAME\tKEYS\tCREATED\tREQUESTS\tERRORS\tPROMPT\tCACHED\tCOMPLETION\tLAST USED", rows)
	return nil
}

func createKey(c *client, o *out, name string) error {
	raw, err := c.do(http.MethodPost, "/admin/keys", map[string]string{"name": name})
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	var k controller.CreatedKey
	if err := json.Unmarshal(raw, &k); err != nil {
		return err
	}
	fmt.Fprintf(o.w, "API key for %s (shown only once, store it now):\n\n  %s\n\n", k.Name, k.Key)
	for _, n := range k.Notes {
		fmt.Fprintln(o.w, "note:", n)
	}
	return nil
}

func stats(c *client, o *out) error {
	raw, err := c.do(http.MethodGet, "/admin/stats", nil)
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	var st controller.Stats
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	cl := st.Cluster
	fmt.Fprintf(o.w, "uptime %s (since %s)\n", (time.Duration(st.UptimeSeconds) * time.Second).String(), st.StartedAt.Local().Format(time.DateTime))
	var states []string
	for s, n := range cl.ByState {
		states = append(states, fmt.Sprintf("%s=%d", s, n))
	}
	sort.Strings(states)
	var models []string
	for m, n := range cl.Models {
		models = append(models, fmt.Sprintf("%s(%d)", m, n))
	}
	sort.Strings(models)
	if len(models) == 0 {
		models = []string{"-"}
	}
	fmt.Fprintf(o.w, "nodes %d [%s], drained %d, ready %d, in flight %d, models %s\n",
		cl.Nodes, strings.Join(states, " "), cl.Drained, cl.ReadyBackends, cl.Inflight, strings.Join(models, " "))
	printTotals(o, "since start", st.SinceStart)
	if st.Lifetime != nil {
		printTotals(o, "since "+st.Lifetime.Since.Local().Format(time.DateTime), *st.Lifetime)
	}
	return nil
}

func printTotals(o *out, title string, t controller.UsageTotals) {
	for _, part := range []struct {
		name string
		m    map[string]controller.UsageCounters
	}{{"API KEY", t.Keys}, {"NODE", t.Nodes}} {
		fmt.Fprintf(o.w, "\n%s, by %s:\n", title, strings.ToLower(part.name))
		if len(part.m) == 0 {
			fmt.Fprintln(o.w, "  no requests")
			continue
		}
		names := make([]string, 0, len(part.m))
		for n := range part.m {
			names = append(names, n)
		}
		sort.Strings(names)
		var rows [][]string
		for _, n := range names {
			u := part.m[n]
			tps := "-"
			if u.AvgGenTPS > 0 {
				tps = fmt.Sprintf("%.1f", u.AvgGenTPS)
			}
			rows = append(rows, []string{n, num(u.Requests), num(u.Errors), num(u.PromptTokens), num(u.CachedPromptTokens),
				num(u.CompletionTokens), tps, ago(u.LastUsed)})
		}
		o.table(part.name+"\tREQUESTS\tERRORS\tPROMPT\tCACHED\tCOMPLETION\tGEN TOK/S\tLAST USED", rows)
	}
}

func showGateway(o *out, raw []byte, err error) error {
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	var g controller.GatewaySettings
	if err := json.Unmarshal(raw, &g); err != nil {
		return err
	}
	thermalLimit := "disabled"
	if g.ThermalLimitC > 0 {
		thermalLimit = fmt.Sprintf("%.1f", g.ThermalLimitC)
	}
	o.table("SETTING\tVALUE", [][]string{
		{"policy", g.Policy}, {"spill", strconv.Itoa(g.AffinitySpill)},
		{"timeout", g.UpstreamTimeout}, {"auth", g.AuthMode}, {"thermal_limit", thermalLimit}})
	return nil
}

func models(c *client, o *out) error {
	raw, err := c.request(http.MethodGet, "/v1/models", nil, c.apiKey)
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	var ms struct {
		Data []struct {
			ID    string `json:"id"`
			Nodes int    `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &ms); err != nil {
		return err
	}
	if len(ms.Data) == 0 {
		fmt.Fprintln(o.w, "no models served (no ready nodes)")
		return nil
	}
	var rows [][]string
	for _, m := range ms.Data {
		rows = append(rows, []string{m.ID, strconv.Itoa(m.Nodes)})
	}
	o.table("MODEL\tNODES", rows)
	return nil
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Round(time.Second).String() + " ago"
}

func num(n int64) string { return strconv.FormatInt(n, 10) }

func pathEscape(s string) string {
	return strings.NewReplacer("/", "%2F", "?", "%3F", "#", "%23", " ", "%20").Replace(s)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
