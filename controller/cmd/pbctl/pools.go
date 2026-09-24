package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/dzaczek/phoneborg/controller"
)

// Node aliases and pools (ADR-014).

// setAlias names a node; "-" or "" clears the alias.
func setAlias(c *client, o *out, id, alias string) error {
	if alias == "-" {
		alias = ""
	}
	raw, err := c.do(http.MethodPatch, "/admin/nodes/"+pathEscape(id), controller.AliasUpdate{Alias: &alias})
	msg := fmt.Sprintf("node %s is now node/%s", id, alias)
	if alias == "" {
		msg = fmt.Sprintf("node %s has no alias", id)
	}
	return o.done(raw, err, msg)
}

func poolsCmd(c *client, o *out, rest []string) error {
	switch {
	case len(rest) == 0:
		return listPools(c, o)
	case rest[0] == "set" && len(rest) >= 2:
		return setPool(c, o, rest[1], rest[2:])
	case rest[0] == "rm" && len(rest) == 2:
		raw, err := c.do(http.MethodDelete, "/admin/pools/"+pathEscape(rest[1]), nil)
		return o.done(raw, err, fmt.Sprintf("pool %s removed", rest[1]))
	}
	return errUsage
}

func getPools(c *client) ([]byte, controller.Pools, error) {
	var ps controller.Pools
	raw, err := c.do(http.MethodGet, "/admin/pools", nil)
	if err != nil {
		return nil, ps, err
	}
	return raw, ps, json.Unmarshal(raw, &ps)
}

func listPools(c *client, o *out) error {
	raw, ps, err := getPools(c)
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	if len(ps.Pools) == 0 {
		fmt.Fprintln(o.w, "no pools")
		return nil
	}
	var rows, members [][]string
	for _, p := range ps.Pools {
		eligible := 0
		for _, m := range p.Members {
			node := m.NodeID
			if m.Alias != "" {
				node = m.Alias + " (" + m.NodeID + ")"
			}
			state := "eligible"
			if m.Eligible {
				eligible++
			} else {
				state = m.Reason
			}
			members = append(members, []string{p.Name, node, dash(m.Model), state})
		}
		minTPS := "-"
		if p.MinGenTPS > 0 {
			minTPS = strconv.FormatFloat(p.MinGenTPS, 'f', -1, 64)
		}
		rows = append(rows, []string{"pool/" + p.Name, p.Routing, strconv.Itoa(eligible), orAny(p.Models), orAny(p.Nodes), orAny(p.Classes),
			minTPS, dash(p.Description)})
	}
	o.table("POOL\tROUTING\tELIGIBLE\tMODELS\tNODES\tCLASSES\tMIN TOK/S\tDESCRIPTION", rows)
	if len(members) > 0 {
		fmt.Fprintln(o.w)
		o.table("POOL\tNODE\tMODEL\tSTATUS", members)
	}
	return nil
}

// setPool creates a pool or changes the given fields of an existing one.
func setPool(c *client, o *out, name string, kvs []string) error {
	_, ps, err := getPools(c)
	if err != nil {
		return err
	}
	p := controller.Pool{Name: name}
	for _, cur := range ps.Pools {
		if cur.Name == name {
			p = cur
		}
	}
	if err := applyPoolSet(&p, kvs); err != nil {
		return err
	}
	raw, err := c.do(http.MethodPut, "/admin/pools/"+pathEscape(name), p)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	eligible := 0
	for _, m := range p.Members {
		if m.Eligible {
			eligible++
		}
	}
	return o.done(raw, nil, fmt.Sprintf("pool/%s saved (%s routing, %d eligible node(s))", p.Name, p.Routing, eligible))
}

// applyPoolSet applies "models=a,b nodes=x,y classes=s,m min_tps=5
// routing=spread desc=text" to p; "-" clears a list.
func applyPoolSet(p *controller.Pool, kvs []string) error {
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("want key=value, got %q", kv)
		}
		switch k {
		case "models":
			p.Models = list(v)
		case "nodes":
			p.Nodes = list(v)
		case "classes":
			p.Classes = list(v)
		case "min_tps":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("min_tps: %w", err)
			}
			p.MinGenTPS = f
		case "routing":
			p.Routing = v
		case "desc", "description":
			p.Description = v
		default:
			return fmt.Errorf("unknown pool setting %q (models, nodes, classes, min_tps, routing, desc)", k)
		}
	}
	p.Members = nil
	return nil
}

// orAny renders a filter list; empty means no restriction.
func orAny(vs []string) string {
	if len(vs) == 0 {
		return "any"
	}
	return strings.Join(vs, ",")
}
