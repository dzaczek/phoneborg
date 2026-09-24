// Package ui embeds the web management panel (static HTML, CSS and ES
// modules, no build step) that the controller serves at /ui/ (ADR-013). The
// panel talks only to the admin API, with the admin token the operator types
// in; it holds no server-side state.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var static embed.FS

// Files is the panel's content, rooted at index.html.
var Files, _ = fs.Sub(static, "static") // "static" is embedded above, so this cannot fail

// csp allows only same-origin scripts, styles and requests: no inline code,
// no CDN, and the page cannot be framed.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// Handler serves the panel. Mount it at "/ui/".
func Handler() http.Handler {
	files := http.StripPrefix("/ui/", http.FileServerFS(Files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No directory listings: the only directory served is the root.
		if r.URL.Path != "/ui/" && strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
