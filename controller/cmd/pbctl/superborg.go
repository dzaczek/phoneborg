package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/dzaczek/phoneborg/controller"
)

// superborgCmd shows or changes Super Borg mode (ADR-020):
//
//	superborg
//	superborg on [orchestrator=<node>|auto] [thinking=on|off]
//	superborg off
func superborgCmd(c *client, o *out, args []string) error {
	if len(args) == 0 {
		raw, err := c.do(http.MethodGet, "/admin/superborg", nil)
		return showSuperborg(o, raw, err)
	}
	if args[0] != "on" && args[0] != "off" {
		return errUsage
	}
	raw, err := c.do(http.MethodGet, "/admin/superborg", nil)
	if err != nil {
		return err
	}
	var st controller.SuperborgStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	sb := st.SuperborgSettings
	sb.Enabled = args[0] == "on"
	for _, kv := range args[1:] {
		k, v, ok := strings.Cut(kv, "=")
		switch {
		case ok && k == "orchestrator":
			if v == "auto" {
				v = ""
			}
			sb.Orchestrator = v
		case ok && k == "thinking" && (v == "on" || v == "off"):
			sb.Thinking = v == "on"
		default:
			return fmt.Errorf("want orchestrator=<node>|auto or thinking=on|off, got %q", kv)
		}
	}
	raw, err = c.do(http.MethodPut, "/admin/superborg", sb)
	return showSuperborg(o, raw, err)
}

func showSuperborg(o *out, raw []byte, err error) error {
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	var st controller.SuperborgStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	onOff := map[bool]string{true: "on", false: "off"}
	orch := st.Orchestrator
	if orch == "" {
		orch = "auto (largest model)"
	}
	active := "none (no ready node)"
	if st.ActiveOrchestrator != "" {
		active = st.ActiveOrchestrator + " " + st.OrchestratorModel
	}
	o.table("SETTING\tVALUE", [][]string{{"superborg", onOff[st.Enabled]}, {"orchestrator", orch},
		{"active", active}, {"thinking", onOff[st.Thinking]}})
	rows := [][]string{}
	for _, w := range st.Workers {
		rows = append(rows, []string{w.NodeID, w.Alias, w.Model, fmt.Sprintf("%.1f", w.GenTPS)})
	}
	fmt.Fprintln(o.w)
	o.table("WORKER\tALIAS\tMODEL\tTOK/S", rows)
	return nil
}
