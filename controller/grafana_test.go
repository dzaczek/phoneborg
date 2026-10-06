package controller

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

func newGrafanaEnv(t *testing.T, grafanaURL string) *env {
	t.Helper()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	reg := NewRegistry(time.Hour, time.Hour, log)
	srv := NewServer(reg, time.Second, GatewayOptions{GrafanaURL: grafanaURL, Devices: DeviceOptions{AutoProvision: true},
		Config: gateway.Config{UpstreamTimeout: 5 * time.Second, MaxAttempts: 2, Cooldown: time.Minute}},
		AdminOptions{Token: testToken}, log)
	return &env{t: t, reg: reg, srv: srv, h: srv.Handler(), logs: logs}
}

func TestGrafanaProxyOff(t *testing.T) {
	e := newGrafanaEnv(t, "")
	var info GrafanaInfo
	e.admin(http.MethodGet, "/admin/grafana", "", http.StatusOK, &info)
	if info.Enabled {
		t.Fatalf("info = %+v, want disabled", info)
	}
	e.admin(http.MethodPost, "/admin/grafana/session", "", http.StatusNotFound, nil)
	if w := e.do(http.MethodGet, "/grafana/d/phoneborg/phoneborg", "", ""); w.Code != http.StatusNotFound {
		t.Errorf("/grafana/ without -grafana-url: %d, want 404", w.Code)
	}
}

func TestGrafanaProxy(t *testing.T) {
	var got *http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Write([]byte("grafana " + r.URL.Path))
	}))
	defer upstream.Close()
	e := newGrafanaEnv(t, upstream.URL)

	if w := e.do(http.MethodGet, "/grafana/d/phoneborg/phoneborg", "", ""); w.Code != http.StatusUnauthorized || got != nil {
		t.Fatalf("no session: %d (upstream called: %v), want 401", w.Code, got != nil)
	}
	w := e.do(http.MethodPost, "/admin/grafana/session", "", testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("session: %d %s", w.Code, w.Body)
	}
	cookie := w.Result().Cookies()[0]
	if cookie.Name != grafanaCookie || cookie.Path != GrafanaPrefix || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v", cookie)
	}

	r := httptest.NewRequest(http.MethodGet, "http://controller:18080/grafana/d/phoneborg/phoneborg?kiosk", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.AddCookie(cookie)
	r.AddCookie(&http.Cookie{Name: "grafana_session", Value: "g"})
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || rec.Body.String() != "grafana /grafana/d/phoneborg/phoneborg" {
		t.Fatalf("proxied: %d %q", rec.Code, rec.Body)
	}
	if got.Header.Get("Authorization") != "" || strings.Contains(got.Header.Get("Cookie"), grafanaCookie) || got.Header.Get("Cookie") != "grafana_session=g" {
		t.Errorf("upstream headers: Authorization %q, Cookie %q", got.Header.Get("Authorization"), got.Header.Get("Cookie"))
	}
	if got.Host != "controller:18080" {
		t.Errorf("upstream Host = %q, want the controller's (Grafana's CSRF check compares it with Origin)", got.Host)
	}

	tampered := *cookie
	tampered.Value = strings.Replace(cookie.Value, ".", "9.", 1)
	r = httptest.NewRequest(http.MethodGet, "/grafana/api/health", nil)
	r.AddCookie(&tampered)
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("tampered cookie: %d, want 401", rec.Code)
	}
	if e.srv.grafana.valid(e.srv.grafana.sign(time.Now().Add(-time.Minute).Unix()), time.Now()) {
		t.Error("an expired session is valid")
	}
}
