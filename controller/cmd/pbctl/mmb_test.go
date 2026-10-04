package main

import (
	"strings"
	"testing"
)

func TestMMBCommand(t *testing.T) {
	ts := newController(t)
	env := map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": token}
	if out, _, code := pbctl(t, env, "mmb"); code != 0 || !strings.Contains(out, "no benchmark runs") {
		t.Fatalf("list: %d %s", code, out)
	}
	// The test controller has no models: the run is refused with a reason.
	if _, stderr, code := pbctl(t, env, "mmb", "run", "nodes=n1"); code != 1 || !strings.Contains(stderr, "no model fits") {
		t.Errorf("run: %d %s", code, stderr)
	}
	if _, stderr, code := pbctl(t, env, "mmb", "run", "colour=red"); code != 1 || !strings.Contains(stderr, "unknown mmb setting") {
		t.Errorf("bad setting: %d %s", code, stderr)
	}
	if _, stderr, code := pbctl(t, env, "mmb", "show", "nope"); code != 1 || !strings.Contains(stderr, "HTTP 404") {
		t.Errorf("show: %d %s", code, stderr)
	}
}
