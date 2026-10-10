package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/dzaczek/phoneborg/controller"
)

// Semantic router and its classes (ADR-033). Class changes read the current
// list, change it and send the whole list back.

func routerCmd(c *client, o *out, rest []string) error {
	switch {
	case len(rest) == 0:
		return showRouter(c, o)
	case len(rest) == 1 && (rest[0] == "on" || rest[0] == "off"):
		on := rest[0] == "on"
		return putRouter(c, o, controller.RouterUpdate{Enabled: &on}, "semantic router "+rest[0])
	case len(rest) == 2 && rest[0] == "classes" && rest[1] == "reset":
		return putRouter(c, o, controller.RouterUpdate{ResetClasses: true}, "router classes reset to the defaults")
	case len(rest) >= 3 && rest[0] == "class":
		return routerClassCmd(c, o, rest[1], rest[2], rest[3:])
	}
	return errUsage
}

func getRouter(c *client) ([]byte, controller.RouterSettings, error) {
	var g controller.GatewaySettings
	raw, err := c.do(http.MethodGet, "/admin/gateway", nil)
	if err != nil {
		return nil, g.Router, err
	}
	return raw, g.Router, json.Unmarshal(raw, &g)
}

func putRouter(c *client, o *out, u controller.RouterUpdate, msg string) error {
	raw, err := c.do(http.MethodPut, "/admin/gateway", controller.GatewayUpdate{Router: &u})
	return o.done(raw, err, msg)
}

func showRouter(c *client, o *out) error {
	raw, r, err := getRouter(c)
	if err != nil {
		return err
	}
	if o.json {
		return o.raw(raw)
	}
	state := "off"
	if r.Enabled {
		state = "on"
	}
	fmt.Fprintf(o.w, "router %s, classifier %s, timeout %s\n\n", state, orDash(r.Classifier), r.Timeout)
	rows := make([][]string, len(r.Classes))
	for i, cl := range r.Classes {
		target := cl.Target
		if target == "" {
			target = "auto"
		}
		rows[i] = []string{cl.Letter, cl.Name, target, cl.Description, strconv.Itoa(len(cl.Examples))}
	}
	o.table("LETTER\tCLASS\tTARGET\tDESCRIPTION\tEXAMPLES", rows)
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// routerClassCmd runs "router class add|set|rm <name> [k=v ...]".
func routerClassCmd(c *client, o *out, verb, name string, kvs []string) error {
	_, r, err := getRouter(c)
	if err != nil {
		return err
	}
	classes := r.Classes
	i := -1
	for j, cl := range classes {
		if cl.Name == name {
			i = j
		}
	}
	var msg string
	switch verb {
	case "add":
		if i >= 0 {
			return fmt.Errorf("class %s exists; change it with: router class set %s ...", name, name)
		}
		cl := controller.RouterClassSettings{}
		cl.Name = name
		if err := applyClassKVs(&cl, kvs); err != nil {
			return err
		}
		classes = append(classes, cl)
		msg = "router class " + name + " added"
	case "set":
		if i < 0 {
			return fmt.Errorf("no router class %s", name)
		}
		if err := applyClassKVs(&classes[i], kvs); err != nil {
			return err
		}
		msg = "router class " + classes[i].Name + " changed"
	case "rm":
		if i < 0 {
			return fmt.Errorf("no router class %s", name)
		}
		if len(kvs) > 0 {
			return errUsage
		}
		classes = append(classes[:i], classes[i+1:]...)
		msg = "router class " + name + " removed"
	default:
		return errUsage
	}
	return putRouter(c, o, controller.RouterUpdate{Classes: &classes}, msg)
}

// applyClassKVs applies name=, desc=, target= and example= (repeatable;
// given examples replace the old ones) to cl.
func applyClassKVs(cl *controller.RouterClassSettings, kvs []string) error {
	var examples []string
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("want key=value, got %q", kv)
		}
		switch k {
		case "name":
			cl.Name = v
		case "desc", "description":
			cl.Description = v
		case "target":
			if v == "auto" {
				v = ""
			}
			cl.Target = v
		case "example":
			examples = append(examples, v)
		default:
			return fmt.Errorf("unknown class field %q (name, desc, target, example)", k)
		}
	}
	if examples != nil {
		cl.Examples = examples
	}
	return nil
}
