package controller

import (
	"net/http"
	"strings"
	"testing"
)

func TestWebRoutes(t *testing.T) {
	e := newEnv(t, testToken, nil, "phone-a")

	w := e.do(http.MethodGet, "/", "", "")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/ui/" {
		t.Fatalf("GET /: %d Location=%q, want redirect to /ui/", w.Code, w.Header().Get("Location"))
	}
	if w := e.do(http.MethodGet, "/ui", "", ""); w.Code/100 != 3 || w.Header().Get("Location") != "/ui/" {
		t.Fatalf("GET /ui: %d Location=%q", w.Code, w.Header().Get("Location"))
	}

	w = e.do(http.MethodGet, "/ui/", "", "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(w.Body.String(), "PhoneBorg") {
		t.Fatalf("GET /ui/: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if w := e.do(http.MethodGet, "/ui/js/app.js", "", ""); w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("GET /ui/js/app.js: %d %q", w.Code, w.Header().Get("Content-Type"))
	}

	w = e.do(http.MethodGet, "/status", "", "")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Body.String(), "phone-a") {
		t.Fatalf("GET /status: %d %s", w.Code, w.Body)
	}

	// Unknown paths stay 404, and the panel needs no token to load (the
	// admin API it calls does).
	if w := e.do(http.MethodGet, "/nope", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("GET /nope: %d", w.Code)
	}
}
