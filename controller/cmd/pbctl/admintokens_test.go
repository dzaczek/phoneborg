package main

import (
	"strings"
	"testing"
)

func TestAdminTokensCommand(t *testing.T) {
	ts := newController(t)
	env := map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": token}
	out, stderr, code := pbctl(t, env, "admin-tokens", "create", "laptop")
	if code != 0 || !strings.Contains(out, `admin token "laptop" (shown once`) {
		t.Fatalf("create: %d %s %s", code, out, stderr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	laptop := lines[1]
	// The new token works on its own.
	if out, _, code := pbctl(t, map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": laptop}, "admin-tokens"); code != 0 ||
		!strings.Contains(out, "laptop") || !strings.Contains(out, "-admin-token-file") {
		t.Fatalf("list with the new token: %d %s", code, out)
	}
	if _, stderr, code := pbctl(t, env, "admin-tokens", "revoke", "admin"); code != 1 || !strings.Contains(stderr, "HTTP 400") {
		t.Errorf("revoke admin: %d %s", code, stderr)
	}
	if out, _, code := pbctl(t, env, "admin-tokens", "revoke", "laptop"); code != 0 || !strings.Contains(out, "revoked") {
		t.Errorf("revoke: %d %s", code, out)
	}
	if _, _, code := pbctl(t, map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": laptop}, "admin-tokens"); code != 1 {
		t.Errorf("revoked token still works: %d", code)
	}
}
