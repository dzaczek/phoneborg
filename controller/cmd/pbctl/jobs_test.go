package main

import (
	"strings"
	"testing"
)

func TestJobsCommand(t *testing.T) {
	ts := newController(t)
	env := map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": token}
	if out, _, code := pbctl(t, env, "jobs"); code != 0 || !strings.Contains(out, "no jobs") {
		t.Fatalf("empty list: %d %s", code, out)
	}
	out, stderr, code := pbctl(t, env, "jobs", "new", "title=Story", "Write", "a", "story")
	if code != 0 || !strings.Contains(out, "queued: Story") {
		t.Fatalf("new: %d %s %s", code, out, stderr)
	}
	id := strings.Fields(out)[1]
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"jobs"}, "Story"},
		{[]string{"jobs", "say", id, "shorter", "please"}, "message sent"},
		{[]string{"jobs", "show", id}, "shorter please"},
		{[]string{"jobs", "result", id}, "# Story"},
		{[]string{"jobs", "cancel", id}, "cancelled"},
		{[]string{"jobs", "rm", id}, "removed"},
	} {
		out, stderr, code := pbctl(t, env, tc.args...)
		if code != 0 || !strings.Contains(out, tc.want) {
			t.Errorf("%v: exit %d, %q %q", tc.args, code, out, stderr)
		}
	}
	if _, stderr, code := pbctl(t, env, "jobs", "doc", id, "plan"); code != 1 || !strings.Contains(stderr, "HTTP 404") {
		t.Errorf("doc of removed job: %d %s", code, stderr)
	}
	if _, _, code := pbctl(t, env, "jobs", "bogus"); code != 2 {
		t.Errorf("bogus: exit %d", code)
	}
}
