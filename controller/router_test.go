package controller

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRouterClassesAPI(t *testing.T) {
	e := newEnv(t, testToken, nil)
	var gs GatewaySettings
	e.admin(http.MethodGet, "/admin/gateway", "", 200, &gs)
	if len(gs.Router.Classes) != 6 || gs.Router.Classes[0].Name != "easy_chat" || gs.Router.Classes[5].Letter != "F" {
		t.Fatalf("default classes = %+v", gs.Router.Classes)
	}

	// The whole list is replaced; letters follow the order, input is normalized.
	e.admin(http.MethodPut, "/admin/gateway", `{"router":{"classes":[
		{"name":"polish","description":" a request\n in Polish ","examples":["Cześć"," ",""],"target":" pool/pl ","letter":"Z"},
		{"name":"other","description":"anything else"}]}}`, 200, &gs)
	want := []RouterClassSettings{{Letter: "A"}, {Letter: "B"}}
	want[0].Name, want[0].Description, want[0].Examples, want[0].Target = "polish", "a request in Polish", []string{"Cześć"}, "pool/pl"
	want[1].Name, want[1].Description = "other", "anything else"
	if got := gs.Router.Classes; len(got) != 2 || got[0].Name != want[0].Name || got[0].Description != want[0].Description ||
		len(got[0].Examples) != 1 || got[0].Examples[0] != "Cześć" || got[0].Target != "pool/pl" || got[0].Letter != "A" || got[1].Letter != "B" {
		t.Fatalf("classes = %+v", got)
	}
	if c := e.srv.gw.Router().Classes; len(c) != 2 || c[0].Target != "pool/pl" {
		t.Fatalf("gateway classes = %+v", c)
	}

	long := strings.Repeat("x", 301)
	for _, bad := range []string{
		`{"router":{"classes":[{"name":"Bad","description":"d"}]}}`,
		`{"router":{"classes":[{"name":"fallback","description":"d"}]}}`,
		`{"router":{"classes":[{"name":"a","description":"d"},{"name":"a","description":"e"}]}}`,
		`{"router":{"classes":[{"name":"a","description":""}]}}`,
		`{"router":{"classes":[{"name":"a","description":"` + long + `"}]}}`,
		`{"router":{"classes":[{"name":"a","description":"d","examples":["1","2","3","4","5","6"]}]}}`,
		`{"router":{"classes":[{"name":"a","description":"d","examples":["` + long + `"]}]}}`,
		`{"router":{"classes":[` + strings.Repeat(`{"name":"a","description":"d"},`, 12) + `{"name":"z","description":"d"}]}}`,
		`{"router":{"enabled":true,"classifier":"node/a","classes":[{"name":"only","description":"d"}]}}`, // one class
	} {
		e.admin(http.MethodPut, "/admin/gateway", bad, http.StatusBadRequest, nil)
	}
	e.admin(http.MethodGet, "/admin/gateway", "", 200, &gs)
	if len(gs.Router.Classes) != 2 {
		t.Fatalf("rejected update changed classes: %+v", gs.Router.Classes)
	}

	e.admin(http.MethodPut, "/admin/gateway", `{"router":{"reset_classes":true}}`, 200, &gs)
	if len(gs.Router.Classes) != 6 || gs.Router.Classes[4].Name != "hard_coding" {
		t.Fatalf("reset classes = %+v", gs.Router.Classes)
	}
}

// The router, classes included, lives in routing.json and survives a restart.
func TestRouterIsStored(t *testing.T) {
	file := filepath.Join(t.TempDir(), "routing.json")
	e := newRoutingEnv(t, file, "a")
	e.admin(http.MethodPut, "/admin/gateway", `{"router":{"enabled":true,"classifier":"node/a","timeout":"45s","classes":[
		{"name":"short","description":"short requests","target":"node/a"},
		{"name":"long","description":"long requests","examples":["write a book"]}]}}`, 200, nil)
	data, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(data), `"router"`) || !strings.Contains(string(data), "write a book") {
		t.Fatalf("routing.json: %v\n%s", err, data)
	}

	e2 := newRoutingEnv(t, file, "a")
	r := e2.srv.gw.Router()
	if !r.Enabled || r.Classifier != "node/a" || r.Timeout.String() != "45s" || len(r.Classes) != 2 ||
		r.Classes[0].Target != "node/a" || r.Classes[1].Examples[0] != "write a book" {
		t.Fatalf("router after restart = %+v", r)
	}

	// An invalid stored router is reported and left at the defaults.
	bad := strings.Replace(string(data), `"name": "short"`, `"name": "Bad Name"`, 1)
	if bad == string(data) {
		t.Fatal("test setup: class name not found in routing.json")
	}
	if err := os.WriteFile(file, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	e3 := newRoutingEnv(t, file, "a")
	if r := e3.srv.gw.Router(); r.Enabled || len(r.Classes) != 6 {
		t.Fatalf("invalid stored router = %+v", r)
	}
	if !strings.Contains(e3.logs.String(), "stored semantic router is invalid") {
		t.Errorf("no log: %s", e3.logs)
	}
}
