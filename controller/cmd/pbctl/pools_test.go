package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/dzaczek/phoneborg/controller"
)

func TestAliasAndPoolCommands(t *testing.T) {
	ts := newController(t)
	env := map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": token}
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"pools"}, []string{"no pools"}},
		{[]string{"nodes", "alias", "n1", "phone-01"}, []string{"node n1 is now node/phone-01"}},
		{[]string{"nodes"}, []string{"phone-01 (n1)"}},
		{[]string{"pools", "set", "fast", "nodes=phone-01", "classes=xs,s", "min_tps=5", "desc=small agents"},
			[]string{"pool/fast saved (spread routing, 0 eligible node(s))"}},
		{[]string{"pools", "set", "fast", "routing=affinity"}, []string{"pool/fast saved (affinity routing"}},
		{[]string{"pools"}, []string{"pool/fast", "affinity", "phone-01", "xs,s", "small agents", "phone-01 (n1)", "not ready"}},
		{[]string{"served"}, []string{"no models served", "KIND", "auto", "pool/fast", "node/phone-01"}},
		{[]string{"pools", "rm", "fast"}, []string{"pool fast removed"}},
		{[]string{"nodes", "alias", "n1", "-"}, []string{"node n1 has no alias"}},
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
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"nodes", "alias", "n1", "auto"}, 1, "HTTP 400"},
		{[]string{"nodes", "alias", "ghost", "x"}, 1, "HTTP 404"},
		{[]string{"pools", "set", "x", "routing=random"}, 1, "routing must be"},
		{[]string{"pools", "set", "x", "color=red"}, 1, "unknown pool setting"},
		{[]string{"pools", "rm", "nope"}, 1, "HTTP 404"},
		{[]string{"pools", "rm"}, 2, "usage:"},
	} {
		_, stderr, code := pbctl(t, env, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d, stderr %q; want %d, %q", tc.args, code, stderr, tc.code, tc.want)
		}
	}
}

func TestApplyPoolSet(t *testing.T) {
	p := controller.Pool{Name: "fast", Models: []string{"a"}, Nodes: []string{"x"}, Members: []controller.PoolMember{{}}}
	if err := applyPoolSet(&p, []string{"models=-", "nodes=y,z", "min_tps=2.5", "routing=spread", "desc=a b"}); err != nil {
		t.Fatal(err)
	}
	if len(p.Models) != 0 || !slices.Equal(p.Nodes, []string{"y", "z"}) || p.MinGenTPS != 2.5 || p.Routing != "spread" ||
		p.Description != "a b" || p.Members != nil {
		t.Fatalf("%+v", p)
	}
	for _, bad := range []string{"min_tps=x", "nodes", "color=red"} {
		if err := applyPoolSet(&p, []string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
