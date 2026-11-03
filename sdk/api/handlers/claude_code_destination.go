package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	claudemodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/claude/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ClaudeCodeDestinationPrefix is the URL prefix of the dedicated Claude Code destination.
// Requests under it are served by the regular routes with all models available, while
// the default destination hides (and optionally rejects) Anthropic models.
const ClaudeCodeDestinationPrefix = "/claude-code"

// AnthropicModelsClaudeCodeOnlyMessage is returned when the default destination rejects an Anthropic model.
const AnthropicModelsClaudeCodeOnlyMessage = "Anthropic models are only available via the Claude Code endpoint"

type claudeCodeDestinationKey struct{}

// WithClaudeCodeDestination marks a request context as received through the Claude Code destination.
func WithClaudeCodeDestination(ctx context.Context) context.Context {
	return context.WithValue(ctx, claudeCodeDestinationKey{}, true)
}

// IsClaudeCodeDestination reports whether ctx (or the Gin request stored in it) came through
// the Claude Code destination.
func IsClaudeCodeDestination(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if marked, _ := ctx.Value(claudeCodeDestinationKey{}).(bool); marked {
		return true
	}
	if c, ok := ctx.Value("gin").(*gin.Context); ok && c != nil && c.Request != nil {
		marked, _ := c.Request.Context().Value(claudeCodeDestinationKey{}).(bool)
		return marked
	}
	return false
}

func isClaudeCodeDestinationGin(c *gin.Context) bool {
	return c != nil && c.Request != nil && IsClaudeCodeDestination(c.Request.Context())
}

func restrictAnthropicModels(cfg *config.SDKConfig) bool {
	return cfg != nil && cfg.ClaudeCode.RestrictAnthropicModels
}

// IsAnthropicModel reports whether model is an Anthropic model. Registry providers and
// ownership are authoritative; the claude- name prefix covers models served through other
// providers or Home, where the local registry does not know the model.
func IsAnthropicModel(model string) bool {
	// Anthropic-format lists cloak other models as claude-fable-5-dd-<reversed>; judge the real ID.
	full := strings.TrimPrefix(claudemodels.ResolveClaudeModelIDPrefix(strings.TrimSpace(model)), "models/")
	base := strings.TrimSpace(thinking.ParseSuffix(full).ModelName)
	if base == "" {
		return false
	}
	if base == "auto" {
		base = util.ResolveAutoModel(base)
	}
	// Routing falls back to the full suffixed ID when the base is not registered, so check both.
	for _, id := range []string{base, full} {
		if isAnthropicRegistryModel(id) {
			return true
		}
	}
	return hasAnthropicModelName(base)
}

func isAnthropicRegistryModel(id string) bool {
	for _, provider := range util.GetProviderName(id) {
		if isAnthropicProvider(provider) {
			return true
		}
	}
	info := registry.GetGlobalRegistry().GetModelInfo(id, "")
	if info == nil {
		return false
	}
	return strings.EqualFold(info.OwnedBy, "anthropic") || isAnthropicProvider(info.Type) || hasAnthropicModelName(info.MetadataModelID)
}

func hasAnthropicModelName(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	return strings.HasPrefix(name, "claude-") || name == "claude"
}

func isAnthropicProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "anthropic":
		return true
	}
	return false
}

// rejectDefaultDestinationAnthropic blocks Anthropic models on client requests that did not
// arrive through the Claude Code destination when claude-code.restrict-anthropic-models is on.
// Internal executions and requests without a client Gin context are not affected.
func (h *BaseAPIHandler) rejectDefaultDestinationAnthropic(ctx context.Context, modelName string, routeDecision modelRouteDecision, execOptions modelExecutionOptions) *interfaces.ErrorMessage {
	if execOptions.InternalSource || !restrictAnthropicModels(h.CurrentConfig()) || ctx == nil {
		return nil
	}
	if c, ok := ctx.Value("gin").(*gin.Context); !ok || c == nil || IsClaudeCodeDestination(ctx) {
		return nil
	}
	if !IsAnthropicModel(modelName) && !isAnthropicProvider(routeDecision.Provider) && !IsAnthropicModel(routeDecision.Model) && !isAnthropicProvider(execOptions.ForcedProvider) {
		return nil
	}
	log.WithField("model", modelName).Info("rejected Anthropic model outside the Claude Code destination")
	body := `{"error":{"message":"","type":"invalid_request_error","code":"model_not_allowed","param":"model"}}`
	body, _ = sjson.Set(body, "error.message", AnthropicModelsClaudeCodeOnlyMessage)
	return &interfaces.ErrorMessage{StatusCode: http.StatusForbidden, Error: errors.New(body)}
}

// filterDefaultDestinationModelList removes Anthropic models from model-list bodies served
// on the default destination. Anthropic-format lists keep them until
// claude-code.restrict-anthropic-models is enabled so unmigrated Claude Code clients keep working.
func (h *BaseAPIHandler) filterDefaultDestinationModelList(c *gin.Context, sourceFormat string, body []byte) []byte {
	if isClaudeCodeDestinationGin(c) {
		return body
	}
	if sourceFormat == "claude" && !restrictAnthropicModels(h.CurrentConfig()) {
		return body
	}
	for _, key := range []string{"data", "models"} {
		list := gjson.GetBytes(body, key)
		if !list.IsArray() {
			continue
		}
		kept := make([]string, 0, len(list.Array()))
		removed := false
		for _, entry := range list.Array() {
			if isAnthropicModelListEntry(entry) {
				removed = true
				continue
			}
			kept = append(kept, entry.Raw)
		}
		if !removed {
			continue
		}
		updated, errSet := sjson.SetRawBytes(body, key, []byte("["+strings.Join(kept, ",")+"]"))
		if errSet != nil {
			log.Warnf("failed to filter Anthropic models from model list: %v", errSet)
			continue
		}
		body = updated
	}
	return body
}

func isAnthropicModelListEntry(entry gjson.Result) bool {
	if strings.EqualFold(entry.Get("owned_by").String(), "anthropic") {
		return true
	}
	for _, field := range []string{"id", "slug", "name"} {
		if value := entry.Get(field).String(); value != "" && IsAnthropicModel(value) {
			return true
		}
	}
	return false
}
