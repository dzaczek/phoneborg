package main

import (
	"strings"
	"testing"
)

func TestSuperborgCommand(t *testing.T) {
	ts := newController(t)
	env := map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": token}
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"superborg"}, []string{"superborg     off", "auto (largest model)", "WORKER"}},
		{[]string{"superborg", "on", "orchestrator=n1", "thinking=on"}, []string{"superborg     on", "orchestrator  n1", "thinking      on"}},
		{[]string{"superborg", "on", "orchestrator=auto"}, []string{"auto (largest model)", "thinking      on"}},
		{[]string{"served"}, []string{"superborg"}},
		{[]string{"superborg", "off"}, []string{"superborg     off"}},
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
		{[]string{"superborg", "on", "orchestrator=ghost"}, 1, "HTTP 400"},
		{[]string{"superborg", "on", "thinking=maybe"}, 1, "want orchestrator="},
		{[]string{"superborg", "toggle"}, 2, "usage:"},
	} {
		_, stderr, code := pbctl(t, env, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d, stderr %q; want %d, %q", tc.args, code, stderr, tc.code, tc.want)
		}
	}
}
