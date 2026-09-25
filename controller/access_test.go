package controller

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/proto"
)

func doAddr(h http.Handler, method, path, body, remoteAddr, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = remoteAddr
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestGatewayAccessLocalControllerLevel checks ADR-017's "local" access mode
// end to end (through NewServer/Handler, not just the gateway package), and
// that it never touches the node protocol, /metrics or /healthz.
func TestGatewayAccessLocalControllerLevel(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	reg := NewRegistry(time.Hour, time.Hour, log)
	trusted, err := gateway.ParseCIDRList("127.0.0.0/8,::1/128")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(reg, time.Second, GatewayOptions{
		AccessMode: gateway.AccessLocal, TrustedCIDRs: trusted,
		Config: gateway.Config{UpstreamTimeout: 5 * time.Second, MaxAttempts: 2, Cooldown: time.Minute},
	}, AdminOptions{}, log)
	h := srv.Handler()

	u, _ := url.Parse(fakeLlama(t).URL)
	port, _ := strconv.Atoi(u.Port())
	reg.Register(proto.RegisterRequest{NodeID: "n1"}, "x")
	_ = reg.ReportBenchmark(proto.BenchmarkReport{NodeID: "n1"})
	_ = reg.Heartbeat(proto.Heartbeat{NodeID: "n1", Runtime: &proto.RuntimeStatus{Model: "m", Ready: true, AdvertiseHost: u.Hostname(), AdvertisePort: port}})

	const remote = "203.0.113.5:1234"
	body := `{"model":"m","messages":[{"role":"user","content":"x"}]}`

	if w := doAddr(h, http.MethodPost, "/v1/chat/completions", body, "127.0.0.1:1234", ""); w.Code != http.StatusOK {
		t.Fatalf("loopback inference: %d %s", w.Code, w.Body)
	}
	w := doAddr(h, http.MethodPost, "/v1/chat/completions", body, remote, "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "remote_requires_api_key") {
		t.Fatalf("remote inference: %d %s", w.Code, w.Body)
	}
	if w := doAddr(h, http.MethodGet, "/v1/models", "", remote, ""); w.Code != http.StatusForbidden {
		t.Fatalf("remote /v1/models: %d %s", w.Code, w.Body)
	}
	if w := doAddr(h, http.MethodGet, "/api/tags", "", remote, ""); w.Code != http.StatusForbidden {
		t.Fatalf("remote /api/tags: %d %s", w.Code, w.Body)
	}

	// The node protocol, /metrics, /healthz and /v1/nodes are never gated,
	// even from a "remote" peer (ADR-017 applies only to inference/listing).
	if w := doAddr(h, http.MethodPost, "/v1/register", `{"node_id":"remote-node"}`, remote, ""); w.Code != http.StatusOK {
		t.Fatalf("remote /v1/register: %d %s", w.Code, w.Body)
	}
	if w := doAddr(h, http.MethodPost, "/v1/heartbeat", `{"node_id":"remote-node"}`, remote, ""); w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("remote /v1/heartbeat: %d %s", w.Code, w.Body)
	}
	if w := doAddr(h, http.MethodGet, "/v1/nodes", "", remote, ""); w.Code != http.StatusOK {
		t.Fatalf("remote /v1/nodes: %d %s", w.Code, w.Body)
	}
	if w := doAddr(h, http.MethodGet, "/metrics", "", remote, ""); w.Code != http.StatusOK {
		t.Fatalf("remote /metrics: %d", w.Code)
	}
	if w := doAddr(h, http.MethodGet, "/healthz", "", remote, ""); w.Code != http.StatusOK {
		t.Fatalf("remote /healthz: %d", w.Code)
	}
}

// TestAdminGatewayReportsAccessMode checks the additive "access" field of
// GET /admin/gateway and that the runtime auth=keys switch overrides it
// (ADR-017).
func TestAdminGatewayReportsAccessMode(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	reg := NewRegistry(time.Hour, time.Hour, log)
	keys := gateway.NewStaticKeys("")
	if _, _, err := keys.Create("alice"); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(reg, time.Second, GatewayOptions{
		Keys: keys, AccessMode: gateway.AccessLocal,
		Config: gateway.Config{UpstreamTimeout: 5 * time.Second, MaxAttempts: 2, Cooldown: time.Minute},
	}, AdminOptions{Token: testToken}, log)
	if got := srv.gatewaySettings().Access; got != gateway.AccessLocal {
		t.Fatalf("access = %q, want %q", got, gateway.AccessLocal)
	}
	if err := keys.Enforce(); err != nil {
		t.Fatal(err)
	}
	if got := srv.gatewaySettings().Access; got != gateway.AccessKeys {
		t.Fatalf("access after auth=keys = %q, want %q", got, gateway.AccessKeys)
	}
}
