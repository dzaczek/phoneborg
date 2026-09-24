package ui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
)

func serve(t *testing.T, p string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("GET /ui/", Handler())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
	return w
}

func TestIndex(t *testing.T) {
	w := serve(t, "/ui/")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /ui/: %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), `<script type="module" src="js/app.js">`) {
		t.Fatal("index.html does not load js/app.js")
	}
	for h, want := range map[string]string{
		"Content-Security-Policy": "script-src 'self'",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
	} {
		if !strings.Contains(w.Header().Get(h), want) {
			t.Errorf("%s = %q, want %q", h, w.Header().Get(h), want)
		}
	}
}

// Every embedded file is served with the right type, and every file that
// index.html or a module references is embedded.
func TestAssetsEmbedded(t *testing.T) {
	types := map[string]string{".html": "text/html", ".css": "text/css", ".js": "text/javascript", ".svg": "image/svg+xml"}
	ref := regexp.MustCompile(`(?:src|href)="([^"#:]+)"|from '(\.[^']+)'`)
	var files []string
	err := fs.WalkDir(Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"index.html", "app.css", "icon.svg", "js/app.js", "js/api.js", "js/views/placement.js"} {
		if _, err := fs.Stat(Files, want); err != nil {
			t.Errorf("%s not embedded", want)
		}
	}
	for _, p := range files {
		w := serve(t, "/ui/"+p)
		if p == "index.html" {
			if w.Code != http.StatusMovedPermanently { // FileServer serves it at /ui/
				t.Errorf("%s: %d", p, w.Code)
			}
		} else if w.Code != http.StatusOK {
			t.Errorf("%s: %d", p, w.Code)
			continue
		}
		if want := types[path.Ext(p)]; want == "" {
			t.Errorf("%s: unexpected file type", p)
		} else if p != "index.html" && !strings.HasPrefix(w.Header().Get("Content-Type"), want) {
			t.Errorf("%s: Content-Type %q, want %s", p, w.Header().Get("Content-Type"), want)
		}
		data, _ := fs.ReadFile(Files, p)
		for _, m := range ref.FindAllStringSubmatch(string(data), -1) {
			target := m[1] + m[2]
			if target == "/status" {
				continue
			}
			if _, err := fs.Stat(Files, path.Join(path.Dir(p), target)); err != nil {
				t.Errorf("%s references missing %s", p, target)
			}
		}
	}
}

func TestNoListingOrMissing(t *testing.T) {
	for _, p := range []string{"/ui/js/", "/ui/js/views/", "/ui/nope.js", "/ui/../ui.go"} {
		if w := serve(t, p); w.Code == http.StatusOK {
			t.Errorf("%s: %d, want an error", p, w.Code)
		}
	}
}
