// Package accountpolicyui serves the maintained account policy controls.
package accountpolicyui

import (
	"embed"
	"net/http"
)

//go:embed assets/*
var assets embed.FS

// Handler serves only the registered assets. API authentication remains
// with the management router; the page collects its management key in memory.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var filename, contentType string
		switch r.URL.Path {
		case "/account-policy.html":
			filename, contentType = "account-policy.html", "text/html; charset=utf-8"
		case "/account-policy.js":
			filename, contentType = "account-policy.js", "text/javascript; charset=utf-8"
		case "/account-policy-dashboard.js":
			filename, contentType = "account-policy-dashboard.js", "text/javascript; charset=utf-8"
		case "/account-policy.css":
			filename, contentType = "account-policy.css", "text/css; charset=utf-8"
		case "/account-policy-icon.svg":
			filename, contentType = "account-policy-icon.svg", "image/svg+xml"
		default:
			http.NotFound(w, r)
			return
		}
		data, errRead := assets.ReadFile("assets/" + filename)
		if errRead != nil {
			http.Error(w, "asset unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(data)
	})
}
