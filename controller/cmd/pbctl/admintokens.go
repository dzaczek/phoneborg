package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/dzaczek/phoneborg/controller"
)

// adminTokensCmd manages named admin tokens (ADR-026):
//
//	admin-tokens
//	admin-tokens create <name>
//	admin-tokens revoke <name>
func adminTokensCmd(c *client, o *out, args []string) error {
	switch {
	case len(args) == 0:
		raw, err := c.do(http.MethodGet, "/admin/tokens", nil)
		if err != nil || o.json {
			return o.done(raw, err, "")
		}
		var l controller.AdminTokens
		if err := json.Unmarshal(raw, &l); err != nil {
			return err
		}
		rows := [][]string{}
		for _, t := range l.Tokens {
			created, kind := "-", "named"
			if !t.Created.IsZero() {
				created = t.Created.Local().Format("2006-01-02 15:04")
			}
			if t.Primary {
				kind = "-admin-token-file"
			}
			rows = append(rows, []string{t.Name, kind, created})
		}
		o.table("NAME\tSOURCE\tCREATED", rows)
		if !l.Persisted {
			fmt.Fprintln(o.w, "\nnote: named tokens are kept in memory only (no -admin-tokens-file or -state-dir)")
		}
		return nil
	case len(args) == 2 && args[0] == "create":
		raw, err := c.do(http.MethodPost, "/admin/tokens", controller.AdminTokenCreate{Name: args[1]})
		if err != nil || o.json {
			return o.done(raw, err, "")
		}
		var t controller.CreatedAdminToken
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		fmt.Fprintf(o.w, "admin token %q (shown once, store it now):\n%s\n", t.Name, t.Token)
		if !t.Persisted {
			fmt.Fprintln(o.w, "note: kept in memory only; it is gone after a controller restart")
		}
		return nil
	case len(args) == 2 && args[0] == "revoke":
		raw, err := c.do(http.MethodDelete, "/admin/tokens/"+pathEscape(args[1]), nil)
		return o.done(raw, err, fmt.Sprintf("admin token %s revoked", args[1]))
	}
	return errUsage
}
