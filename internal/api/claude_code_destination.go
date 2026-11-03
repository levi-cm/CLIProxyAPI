package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/client/grokbuild"
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

// filterDefaultDestinationHomeEntries drops Home catalog entries from Anthropic provider
// sections on the default destination, before the section information is lost in formatting.
// Anthropic-format lists keep them until claude-code.restrict-anthropic-models is enabled.
func (s *Server) filterDefaultDestinationHomeEntries(c *gin.Context, entries []homeModelEntry) []homeModelEntry {
	if c == nil || c.Request == nil || handlers.IsClaudeCodeDestination(c.Request.Context()) {
		return entries
	}
	cfg := s.getConfig()
	restricted := cfg != nil && cfg.ClaudeCode.RestrictAnthropicModels
	if !restricted && isHomeAnthropicFormatModelsRequest(c) {
		return entries
	}
	kept := make([]homeModelEntry, 0, len(entries))
	for _, entry := range entries {
		if !isAnthropicHomeEntry(entry) {
			kept = append(kept, entry)
		}
	}
	return kept
}

// isHomeAnthropicFormatModelsRequest mirrors unifiedModelsHandler: only plain /v1/models requests
// from Anthropic clients are answered in Anthropic format (not Codex, Grok or Gemini catalogs).
func isHomeAnthropicFormatModelsRequest(c *gin.Context) bool {
	if _, codexCatalog := c.Request.URL.Query()["client_version"]; codexCatalog {
		return false
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1beta") || grokbuild.IsGrokShellUserAgent(c.GetHeader("User-Agent")) {
		return false
	}
	return isAnthropicModelsRequest(c)
}

func isAnthropicHomeEntry(entry homeModelEntry) bool {
	if strings.EqualFold(entry.ownedBy, "anthropic") || handlers.IsAnthropicModel(entry.id) {
		return true
	}
	for _, provider := range entry.providers {
		switch strings.ToLower(strings.TrimSpace(provider)) {
		case "claude", "anthropic":
			return true
		}
	}
	return false
}
