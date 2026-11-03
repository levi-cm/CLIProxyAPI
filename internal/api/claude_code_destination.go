package api

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

// wrapClaudeCodeDestinationHandler serves /claude-code/v1/... with the regular /v1 routes,
// handlers and auth middleware. The prefix is stripped before Gin routing and the request
// context is marked so that the default-destination Anthropic filters do not apply.
func wrapClaudeCodeDestinationHandler(next http.Handler) http.Handler {
	if next == nil {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r == nil || r.URL == nil {
			next.ServeHTTP(w, r)
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, handlers.ClaudeCodeDestinationPrefix)
		if !ok || (rest != "/v1" && !strings.HasPrefix(rest, "/v1/")) {
			next.ServeHTTP(w, r)
			return
		}
		r = r.WithContext(handlers.WithClaudeCodeDestination(r.Context()))
		u := *r.URL
		u.Path = rest
		if u.RawPath != "" {
			u.RawPath = strings.TrimPrefix(u.RawPath, handlers.ClaudeCodeDestinationPrefix)
		}
		r.URL = &u
		r.RequestURI = u.RequestURI()
		next.ServeHTTP(w, r)
	})
}
