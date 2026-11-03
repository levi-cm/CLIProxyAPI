package handlers

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestDestinationAliasNormalization(t *testing.T) {
	// Resolve auto using the live fixture registry selection, so the check is independent of list ordering.
	id := t.Name()
	registry.GetGlobalRegistry().RegisterClient(id, "fixture", []*registry.ModelInfo{{ID: "dest-review-auto", Created: 1 << 62, Object: "model", OwnedBy: "fixture"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	selected := util.ResolveAutoModel("auto")
	if selected != "dest-review-auto" {
		t.Fatalf("auto selected %q", selected)
	}
	m := auth.NewManager(nil, nil, nil)
	m.SetConfig(&config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "fixture", Models: []config.OpenAICompatibilityModel{{Name: "gpt-test", Alias: selected}, {Name: "claude-test", Alias: selected}}}}})
	h := NewBaseAPIHandlers(nil, m)
	for _, model := range []string{"auto", "auto(high)", "models/" + selected, selected + "(high)"} {
		if !h.isAnthropicModel(model) {
			t.Errorf("%q not detected", model)
		}
	}
}

func TestDestinationGPTAliasShadowing(t *testing.T) {
	for _, tt := range []struct {
		name, request string
		cfg           config.Config
	}{
		{name: "forced namespace", request: "dest-review-gpt", cfg: config.Config{SDKConfig: config.SDKConfig{ForceModelPrefix: true}, OpenAICompatibility: []config.OpenAICompatibility{{Name: "fixture-gpt", Models: []config.OpenAICompatibilityModel{{Name: "gpt-test", Alias: "dest-review-gpt"}}}, {Name: "fixture-claude", Prefix: "team", Models: []config.OpenAICompatibilityModel{{Name: "claude-test", Alias: "dest-review-gpt"}}}}}},
		{name: "suffix precedence", request: "dest-review-gpt(high)", cfg: config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "fixture", Models: []config.OpenAICompatibilityModel{{Name: "claude-test", Alias: "dest-review-gpt"}, {Name: "gpt-test", Alias: "dest-review-gpt(high)"}}}}}},
		{name: "disabled provider", request: "dest-review-gpt", cfg: config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "fixture", Disabled: true, Models: []config.OpenAICompatibilityModel{{Name: "claude-test", Alias: "dest-review-gpt"}}}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := auth.NewManager(nil, nil, nil)
			m.SetConfig(&tt.cfg)
			if NewBaseAPIHandlers(nil, m).isAnthropicModel(tt.request) {
				t.Fatal("GPT-only alias classified as Anthropic")
			}
		})
	}
}

func BenchmarkDestinationModelList60(b *testing.B) {
	cfg := &config.Config{}
	body := `{"data":[`
	for i := 0; i < 60; i++ {
		model := fmt.Sprintf("dest-bench-%d", i)
		cfg.OpenAICompatibility = append(cfg.OpenAICompatibility, config.OpenAICompatibility{Name: model, Models: []config.OpenAICompatibilityModel{{Name: "gpt-test", Alias: model}}})
		if i > 0 {
			body += ","
		}
		body += fmt.Sprintf(`{"id":%q,"object":"model"}`, model)
	}
	body += "]}"
	m := auth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	h := NewBaseAPIHandlers(nil, m)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v1/models", nil)
	payload := []byte(body)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.filterDefaultDestinationModelList(c, "openai", payload)
	}
}

func TestDestinationAnthropicListCursors(t *testing.T) {
	cfg := &config.SDKConfig{}
	cfg.ClaudeCode.RestrictAnthropicModels = true
	h := NewBaseAPIHandlers(cfg, nil)
	for _, tt := range []struct{ body, want string }{
		{body: `{"data":[{"id":"claude-review-first"},{"id":"gpt-review"},{"id":"claude-review-last"}],"first_id":"claude-review-first","last_id":"claude-review-last","has_more":false}`, want: "gpt-review"},
		{body: `{"data":[{"id":"claude-review-only"}],"first_id":"claude-review-only","last_id":"claude-review-only","has_more":false}`},
	} {
		body := h.filterDefaultDestinationModelList(nil, "claude", []byte(tt.body))
		for _, key := range []string{"first_id", "last_id"} {
			if got := gjson.GetBytes(body, key).String(); got != tt.want {
				t.Fatalf("%s=%q, want %q; body=%s", key, got, tt.want, body)
			}
		}
	}
}

func BenchmarkDestinationAliasRequest60(b *testing.B) {
	cfg := &config.Config{}
	for i := 0; i < 60; i++ {
		model := fmt.Sprintf("dest-bench-%d", i)
		cfg.OpenAICompatibility = append(cfg.OpenAICompatibility, config.OpenAICompatibility{Name: model, Models: []config.OpenAICompatibilityModel{{Name: "gpt-test", Alias: model}}})
	}
	m := auth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	h := NewBaseAPIHandlers(nil, m)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.isAnthropicModel("dest-bench-59")
	}
}
