package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/dzaczek/phoneborg/controller"
)

// USB device auto-detection (ADR-019).

func devicesCmd(c *client, o *out, rest []string) error {
	switch {
	case len(rest) == 0:
		return listDevices(c, o)
	case rest[0] == "auto" && len(rest) == 2 && (rest[1] == "on" || rest[1] == "off"):
		raw, err := c.do(http.MethodPut, "/admin/devices", map[string]bool{"auto_provision": rest[1] == "on"})
		return o.done(raw, err, "auto-provision "+rest[1])
	case rest[0] == "provision" && len(rest) == 2:
		raw, err := c.do(http.MethodPost, "/admin/devices/"+pathEscape(rest[1])+"/provision", nil)
		return o.done(raw, err, "provision requested for "+rest[1])
	}
	return errUsage
}

func listDevices(c *client, o *out) error {
	raw, err := c.do(http.MethodGet, "/admin/devices", nil)
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	var ds controller.Devices
	if err := json.Unmarshal(raw, &ds); err != nil {
		return err
	}
	fmt.Fprintf(o.w, "auto-provision: %s\n", onOff(ds.AutoProvision))
	if len(ds.Devices) == 0 {
		fmt.Fprintln(o.w, "no devices detected (pcprov watch is not running, or not reporting: pass -controller-url/-admin-token-file)")
		return nil
	}
	var rows [][]string
	for _, d := range ds.Devices {
		status := d.Status
		if d.Status == "provisioning" && d.Step != "" {
			status += ":" + d.Step
		}
		if d.Pending {
			status += " (queued)"
		}
		node := "-"
		if d.NodeID != "" {
			node = d.NodeID
			if d.Alias != "" {
				node = d.Alias + " (" + d.NodeID + ")"
			}
		}
		host := d.Host
		if d.Stale {
			host += " (not reporting)"
		}
		rows = append(rows, []string{d.Serial, d.ADBState, dash(d.Model), status, dash(d.Hint), node, host, ago(d.LastSeen)})
	}
	o.table("SERIAL\tADB\tMODEL\tSTATUS\tHINT\tNODE\tHOST\tLAST SEEN", rows)
	return nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
