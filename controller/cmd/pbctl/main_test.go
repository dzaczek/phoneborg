package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller"
	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/proto"
)

const token = "pbctl-test-admin-token"

// newController runs a real controller with one registered node "n1".
func newController(t *testing.T) *httptest.Server {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := controller.NewRegistry(time.Hour, time.Hour, log)
	srv := controller.NewServer(reg, time.Second, controller.GatewayOptions{
		Keys:   gateway.NewStaticKeys(filepath.Join(t.TempDir(), "keys")),
		Config: gateway.Config{UpstreamTimeout: time.Second, MaxAttempts: 1, Cooldown: time.Second}},
		controller.AdminOptions{Token: token}, log)
	reg.Register(proto.RegisterRequest{NodeID: "n1", Inventory: proto.Inventory{Manufacturer: "Xiaomi", Model: "Mi 8"}}, "x")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func pbctl(t *testing.T, env map[string]string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, func(k string) string { return env[k] }, &out, &errb)
	return out.String(), errb.String(), code
}

func TestCommands(t *testing.T) {
	ts := newController(t)
	env := map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": token}

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"nodes"}, []string{"NODE", "TOK/S", "n1", "BENCHMARKING", "Xiaomi Mi 8"}},
		{[]string{"gateway", "set", "thermal_limit=60"}, []string{"thermal_limit", "60"}},
		{[]string{"gateway"}, []string{"thermal_limit", "60"}},
		{[]string{"gateway", "set", "router=on", "router_classifier=node/n1", "router_timeout=30s"},
			[]string{"router", "on", "node/n1", "30s", "6 (pbctl router)"}},
		{[]string{"router"}, []string{"router on, classifier node/n1", "LETTER", "A", "easy_chat", "auto", "F", "hard_reasoning"}},
		{[]string{"router", "class", "add", "polish", "desc=a request in Polish", "target=pool/pl", "example=Cześć", "example=Dzień dobry"},
			[]string{"router class polish added"}},
		{[]string{"router"}, []string{"G", "polish", "pool/pl", "a request in Polish", "2"}},
		{[]string{"router", "class", "set", "polish", "name=pl", "target=auto"}, []string{"router class pl changed"}},
		{[]string{"router", "class", "rm", "easy_chat"}, []string{"router class easy_chat removed"}},
		{[]string{"router"}, []string{"A", "easy_writing", "F", "pl"}},
		{[]string{"router", "off"}, []string{"semantic router off"}},
		{[]string{"router", "classes", "reset"}, []string{"router classes reset"}},
		{[]string{"router"}, []string{"router off", "easy_chat"}},
		{[]string{"drain", "n1"}, []string{"node n1 drained"}},
		{[]string{"nodes"}, []string{"DRAINED"}},
		{[]string{"undrain", "n1"}, []string{"node n1 undrained"}},
		{[]string{"keys", "create", "ci"}, []string{"API key for ci", "pb-", "gateway is open"}},
		{[]string{"keys"}, []string{"auth mode: open", "persisted", "ci"}},
		{[]string{"gateway", "set", "policy=least_inflight", "spill=4", "timeout=90s"}, []string{"least_inflight", "4", "1m30s"}},
		{[]string{"gateway", "set", "auth=keys"}, []string{"auth", "keys"}},
		{[]string{"gateway"}, []string{"policy", "least_inflight"}},
		{[]string{"stats"}, []string{"uptime", "nodes 1", "since start"}},
		{[]string{"keys", "revoke", "ci"}, []string{"keys of ci revoked"}},
		{[]string{"forget", "n1"}, []string{"node n1 forgotten"}},
		{[]string{"nodes"}, []string{"no nodes"}},
	} {
		stdout, stderr, code := pbctl(t, env, tc.args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.args, code, stderr)
		}
		for _, w := range tc.want {
			if !strings.Contains(stdout, w) {
				t.Errorf("%v: output lacks %q:\n%s", tc.args, w, stdout)
			}
		}
	}
}

func TestJSONOutputAndErrors(t *testing.T) {
	ts := newController(t)
	env := map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": token}

	stdout, _, code := pbctl(t, env, "nodes", "-json")
	var ns []controller.AdminNode
	if code != 0 || json.Unmarshal([]byte(stdout), &ns) != nil || len(ns) != 1 {
		t.Fatalf("nodes -json: %d %s", code, stdout)
	}

	for _, tc := range []struct {
		env  map[string]string
		args []string
		code int
		want string
	}{
		{env, []string{"drain", "ghost"}, 1, "HTTP 404: unknown node"},
		{map[string]string{"PHONEBORG_URL": ts.URL}, []string{"nodes"}, 1, "no admin token"},
		{map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": "wrong"}, []string{"nodes"}, 1, "HTTP 401"},
		{env, []string{"gateway", "set", "policy=random"}, 1, "policy must be"},
		{env, []string{"gateway", "set", "bogus=1"}, 1, "unknown setting"},
		{env, []string{"router", "class", "add", "Bad", "desc=x"}, 1, "router class name"},
		{env, []string{"router", "class", "add", "easy_chat", "desc=x"}, 1, "exists"},
		{env, []string{"router", "class", "set", "ghost", "desc=x"}, 1, "no router class ghost"},
		{env, []string{"router", "class", "set", "easy_chat", "colour=red"}, 1, "unknown class field"},
		{env, []string{"router", "maybe"}, 2, "usage:"},
		{env, []string{"frobnicate"}, 2, "usage:"},
		{env, []string{"drain"}, 2, "usage:"},
		{env, nil, 2, "usage:"},
	} {
		_, stderr, code := pbctl(t, tc.env, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d, stderr %q; want %d, %q", tc.args, code, stderr, tc.code, tc.want)
		}
	}
}

func TestTokenFileAndModels(t *testing.T) {
	ts := newController(t)
	tf := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tf, []byte("# admin\n"+token+"\n"), 0o600)
	if _, stderr, code := pbctl(t, map[string]string{"PHONEBORG_URL": ts.URL}, "-token-file", tf, "gateway"); code != 0 {
		t.Fatalf("token file: %s", stderr)
	}
	// served needs no admin token; with no ready nodes it lists nothing.
	if stdout, stderr, code := pbctl(t, nil, "-url", ts.URL, "served"); code != 0 || !strings.Contains(stdout, "no models") {
		t.Fatalf("served: %d %s %s", code, stdout, stderr)
	}
}

func TestParseGatewaySet(t *testing.T) {
	u, err := parseGatewaySet([]string{"policy=affinity", "spill=3", "timeout=600s", "auth=keys", "thermal_limit=60"})
	if err != nil || *u.Policy != "affinity" || *u.AffinitySpill != 3 || *u.UpstreamTimeout != "600s" || *u.AuthMode != "keys" || *u.ThermalLimitC != 60 {
		t.Fatalf("%+v %v", u, err)
	}
	u, err = parseGatewaySet([]string{"router=on", "router_classifier=node/mi8", "router_timeout=30s"})
	if r := u.Router; err != nil || r == nil || !*r.Enabled || *r.Classifier != "node/mi8" || *r.Timeout != "30s" {
		t.Fatalf("router: %+v %v", u.Router, err)
	}
	for _, bad := range [][]string{{"spill=x"}, {"policy"}, {"policy="}, {"color=red"}, {"thermal_limit=hot"}, {"router=yes"}, {"router_easy=pool/x"}} {
		if _, err := parseGatewaySet(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestTokenFromEnvIsTrimmed(t *testing.T) {
	ts := newController(t)
	env := map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": token + "\n"}
	if _, stderr, code := pbctl(t, env, "nodes"); code != 0 {
		t.Fatalf("token with a trailing newline: %d %s", code, stderr)
	}
}
