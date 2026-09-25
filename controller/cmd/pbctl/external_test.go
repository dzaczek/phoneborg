package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dzaczek/phoneborg/controller"
)

func TestExternalCommands(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"object":"list","data":[{"id":"qwen-7b"},{"id":"llama-8b"}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"1 2"}}],"usage":{"completion_tokens":32},"timings":{"predicted_per_second":42.5,"prompt_per_second":300}}`)
	}))
	t.Cleanup(up.Close)
	keyFile := filepath.Join(t.TempDir(), "mac-key")
	if err := os.WriteFile(keyFile, []byte("# LM Studio\nsk-local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := newController(t)
	env := map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": token}
	for _, tc := range []struct {
		args []string
		want []string
		not  []string
	}{
		{[]string{"external"}, []string{"no external nodes"}, nil},
		{[]string{"external", "add", "mac", up.URL + "/v1", "key-file=" + keyFile, "concurrency=2", "ctx=32768"},
			[]string{"external node mac saved (node/mac, node id ext:mac): ACTIVE, 2 model(s)"}, nil},
		{[]string{"external"}, []string{"NAME", "mac", up.URL, "ACTIVE", "qwen-7b,llama-8b", "0/2", "32768", "yes"}, []string{"sk-local"}},
		{[]string{"external", "-json"}, []string{`"has_api_key": true`}, []string{"sk-local"}},
		{[]string{"external", "selftest", "mac"}, []string{"MODEL", "llama-8b", "42.5", "300.0"}, nil},
		{[]string{"nodes"}, []string{"KIND", "n1", "phone", "mac (ext:mac)", "external", up.URL, "qwen-7b (+1)", "42.5", "0/2"}, nil},
		{[]string{"drain", "ext:mac"}, []string{"node ext:mac drained"}, nil},
		{[]string{"external"}, []string{"ACTIVE DRAINED"}, nil},
		{[]string{"undrain", "ext:mac"}, []string{"node ext:mac undrained"}, nil},
		{[]string{"external", "add", "mac", up.URL, "models=llama-8b", "speed=12"}, []string{"ACTIVE, 1 model(s)"}, nil},
		{[]string{"external"}, []string{"llama-8b", "12.0 (hint)", "yes"}, nil},
		{[]string{"served"}, []string{"llama-8b", "node/mac"}, nil},
		{[]string{"external", "rm", "mac"}, []string{"external node mac removed"}, nil},
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
		for _, n := range tc.not {
			if strings.Contains(stdout, n) {
				t.Errorf("%v: output contains %q:\n%s", tc.args, n, stdout)
			}
		}
	}
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"external", "add", "Bad", up.URL}, 1, "HTTP 400"},
		{[]string{"external", "add", "x", up.URL, "concurrency=two"}, 1, "concurrency:"},
		{[]string{"external", "add", "x", up.URL, "color=red"}, 1, "unknown external setting"},
		{[]string{"external", "add", "x", up.URL, "key-file=/nonexistent"}, 1, "key-file:"},
		{[]string{"external", "rm", "nope"}, 1, "HTTP 404"},
		{[]string{"external", "add", "x"}, 2, "usage:"},
	} {
		_, stderr, code := pbctl(t, env, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d, stderr %q; want %d, %q", tc.args, code, stderr, tc.code, tc.want)
		}
	}
}

func TestParseExternalAdd(t *testing.T) {
	spec, err := parseExternalAdd("http://h:1", []string{"models=a,b", "concurrency=3", "ctx=4096", "speed=9.5", "key-file=-"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.URL != "http://h:1" || strings.Join(spec.Models, ",") != "a,b" || spec.MaxConcurrency != 3 || spec.CtxSize != 4096 ||
		spec.SpeedTPS != 9.5 || spec.APIKey == nil || *spec.APIKey != "" {
		t.Fatalf("%+v", spec)
	}
	if spec, _ := parseExternalAdd("http://h:1", nil); spec.APIKey != nil {
		t.Fatal("no key-file must keep the current key")
	}
}

func TestExternalNodeRows(t *testing.T) {
	rows := externalNodeRows([]controller.External{{Name: "pc", NodeID: "ext:pc", URL: "http://pc:8080", State: controller.ExternalOffline,
		MaxConcurrency: 1, Drained: true}})
	if got := strings.Join(rows[0], "|"); got != "pc (ext:pc)|external|-|OFFLINE|DRAINED|http://pc:8080|-|-|-|-|0/1|0|never" {
		t.Fatalf("row %s", got)
	}
}
