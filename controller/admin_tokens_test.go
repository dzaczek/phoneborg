package controller

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

func TestNamedAdminTokens(t *testing.T) {
	e := newEnv(t, testToken, nil, "a")
	var tok CreatedAdminToken
	e.admin(http.MethodPost, "/admin/tokens", `{"name":"laptop"}`, 201, &tok)
	if tok.Name != "laptop" || len(tok.Token) != 64 || tok.Persisted {
		t.Fatalf("created %+v", tok)
	}
	e.admin(http.MethodPost, "/admin/tokens", `{"name":"laptop"}`, 409, nil)
	e.admin(http.MethodPost, "/admin/tokens", `{"name":"admin"}`, 409, nil)
	e.admin(http.MethodPost, "/admin/tokens", `{"name":"Bad Name"}`, 400, nil)

	// The new token works and the audit log names it.
	w := e.do(http.MethodPatch, "/admin/nodes/a", `{"alias":"phone"}`, tok.Token)
	if w.Code != 200 {
		t.Fatalf("laptop token: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(e.logs.String(), `"principal":"laptop","action":"node_alias"`) {
		t.Errorf("audit does not name the token:\n%s", e.logs)
	}
	var list AdminTokens
	e.admin(http.MethodGet, "/admin/tokens", "", 200, &list)
	if len(list.Tokens) != 2 || !list.Tokens[0].Primary || list.Tokens[1].Name != "laptop" {
		t.Fatalf("list %+v", list)
	}
	e.admin(http.MethodDelete, "/admin/tokens/admin", "", 400, nil)
	e.admin(http.MethodDelete, "/admin/tokens/laptop", "", 204, nil)
	e.admin(http.MethodDelete, "/admin/tokens/laptop", "", 404, nil)
	if w := e.do(http.MethodGet, "/admin/nodes", "", tok.Token); w.Code != 401 {
		t.Errorf("revoked token still works: %d", w.Code)
	}
}

func TestNamedAdminTokensPersist(t *testing.T) {
	file := filepath.Join(t.TempDir(), "admin-tokens")
	newServerWith := func() *Server {
		tokens, err := LoadAdminTokens(file)
		if err != nil {
			t.Fatal(err)
		}
		log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		return NewServer(NewRegistry(time.Hour, time.Hour, log), time.Second,
			GatewayOptions{Config: gateway.Config{UpstreamTimeout: time.Second, MaxAttempts: 1, Cooldown: time.Second}},
			AdminOptions{Token: testToken, TokensFile: file, Tokens: tokens}, log)
	}
	created, err := newServerWith().adminTokens.create("server", time.Now())
	if err != nil || !created.Persisted {
		t.Fatalf("%+v %v", created, err)
	}
	data, _ := os.ReadFile(file)
	if strings.Contains(string(data), created.Token) || !strings.Contains(string(data), "server sha256:") {
		t.Fatalf("file holds the token itself or no hash:\n%s", data)
	}
	if name, ok := newServerWith().adminTokens.match(created.Token); !ok || name != "server" {
		t.Errorf("after reload: %q %v", name, ok)
	}
	if err := os.WriteFile(file, []byte("broken line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAdminTokens(file); err == nil {
		t.Error("broken file accepted")
	}
	if tokens, err := LoadAdminTokens(filepath.Join(t.TempDir(), "missing")); err != nil || len(tokens) != 0 {
		t.Errorf("missing file: %v %v", tokens, err)
	}
}
