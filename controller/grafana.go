package controller

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Grafana in the panel (docs/DECISIONS.md ADR-031): the controller serves
// Grafana under /grafana/ on its own port, so the panel can show dashboards
// in iframes from its own origin (its CSP allows no other) and Grafana needs
// no port of its own. An iframe cannot send the panel's bearer token, so the
// panel trades it for a short-lived cookie scoped to /grafana/.

const (
	// GrafanaPrefix is where the controller serves Grafana. Grafana must be
	// configured with serve_from_sub_path and a root_url ending in it.
	GrafanaPrefix       = "/grafana/"
	grafanaCookie       = "phoneborg_grafana"
	grafanaSessionTTL   = 12 * time.Hour
	grafanaDashboardUID = "phoneborg"
)

// GrafanaInfo is GET /admin/grafana and POST /admin/grafana/session.
type GrafanaInfo struct {
	Enabled      bool   `json:"enabled"`
	Prefix       string `json:"prefix,omitempty"`
	DashboardUID string `json:"dashboard_uid,omitempty"`
}

type grafanaProxy struct {
	proxy *httputil.ReverseProxy
	key   []byte // signs session cookies; new on every start
}

// newGrafanaProxy proxies to rawURL (e.g. http://127.0.0.1:3000); "" = off.
func newGrafanaProxy(rawURL string) (*grafanaProxy, error) {
	if rawURL == "" {
		return nil, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("-grafana-url %q: want http(s)://host:port", rawURL)
	}
	u.Path = ""
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	p := httputil.NewSingleHostReverseProxy(u)
	// The Host header stays the controller's: Grafana compares it with the
	// Origin of POST requests (CSRF check), which is the controller's too.
	return &grafanaProxy{proxy: p, key: key}, nil
}

func (g *grafanaProxy) sign(expires int64) string {
	m := hmac.New(sha256.New, g.key)
	m.Write([]byte(strconv.FormatInt(expires, 10)))
	return strconv.FormatInt(expires, 10) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (g *grafanaProxy) valid(cookie string, now time.Time) bool {
	exp, _, ok := strings.Cut(cookie, ".")
	if !ok {
		return false
	}
	e, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || now.Unix() >= e {
		return false
	}
	return hmac.Equal([]byte(g.sign(e)), []byte(cookie))
}

// serve proxies a request that carries a valid session cookie.
func (g *grafanaProxy) serve(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(grafanaCookie)
	if err != nil || !g.valid(c.Value, time.Now()) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintln(w, "Open Grafana from the PhoneBorg panel (Dashboards); it signs you in here.")
		return
	}
	// Never pass the controller's credentials on; Grafana's own cookies stay.
	r.Header.Del("Authorization")
	var keep []string
	for _, ck := range r.Cookies() {
		if ck.Name != grafanaCookie {
			keep = append(keep, ck.Name+"="+ck.Value)
		}
	}
	r.Header.Del("Cookie")
	if len(keep) > 0 {
		r.Header.Set("Cookie", strings.Join(keep, "; "))
	}
	g.proxy.ServeHTTP(w, r)
}

func (s *Server) grafanaInfo() GrafanaInfo {
	if s.grafana == nil {
		return GrafanaInfo{}
	}
	return GrafanaInfo{Enabled: true, Prefix: GrafanaPrefix, DashboardUID: grafanaDashboardUID}
}

func (s *Server) registerGrafana(mux *http.ServeMux) {
	if s.grafana == nil {
		return
	}
	mux.HandleFunc(GrafanaPrefix, s.grafana.serve)
	mux.Handle(strings.TrimSuffix(GrafanaPrefix, "/"), http.RedirectHandler(GrafanaPrefix, http.StatusFound))
}

func (s *Server) registerGrafanaAdmin(add func(pattern, action string, fn http.HandlerFunc)) {
	add("GET /admin/grafana", "grafana_get", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.grafanaInfo())
	})
	add("POST /admin/grafana/session", "grafana_session", func(w http.ResponseWriter, r *http.Request) {
		if s.grafana == nil {
			httpError(w, http.StatusNotFound, "Grafana is not set up; start the controller with -grafana-url")
			return
		}
		exp := time.Now().Add(grafanaSessionTTL)
		http.SetCookie(w, &http.Cookie{Name: grafanaCookie, Value: s.grafana.sign(exp.Unix()), Path: GrafanaPrefix,
			Expires: exp, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
		writeJSON(w, http.StatusOK, s.grafanaInfo())
	})
}
