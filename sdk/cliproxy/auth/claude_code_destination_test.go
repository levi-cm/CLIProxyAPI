package auth

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestDestinationConfiguredAliasMatchesExecution(t *testing.T) {
	for _, tt := range []struct {
		name, prefix, request string
		force                 bool
		models                []config.OpenAICompatibilityModel
		want                  []string
	}{
		{name: "prefixed pool", prefix: "team", request: "team/alias(high)", models: []config.OpenAICompatibilityModel{{Name: "gpt-test", Alias: "alias"}, {Name: "claude-test", Alias: "alias"}}, want: []string{"gpt-test(high)", "claude-test(high)"}},
		{name: "forced prefix", prefix: "team", force: true, request: "team/alias", models: []config.OpenAICompatibilityModel{{Name: "claude-test", Alias: "alias"}}, want: []string{"claude-test"}},
		{name: "unrelated forced prefix", prefix: "team", force: true, request: "alias", models: []config.OpenAICompatibilityModel{{Name: "claude-test", Alias: "alias"}}},
		{name: "prefix case sensitive", prefix: "team", request: "TEAM/alias", models: []config.OpenAICompatibilityModel{{Name: "claude-test", Alias: "alias"}}},
		{name: "alias case insensitive", request: "ALIAS", models: []config.OpenAICompatibilityModel{{Name: "claude-test", Alias: "alias"}}, want: []string{"claude-test"}},
		{name: "full suffix pool", request: "alias(high)", models: []config.OpenAICompatibilityModel{{Name: "gpt-test", Alias: "alias(high)"}, {Name: "claude-test", Alias: "alias(high)"}}, want: []string{"gpt-test(high)", "claude-test(high)"}},
		{name: "exact suffix wins", request: "alias(high)", models: []config.OpenAICompatibilityModel{{Name: "claude-test", Alias: "alias"}, {Name: "gpt-test", Alias: "alias(high)"}}, want: []string{"gpt-test(high)"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.SetConfig(&config.Config{SDKConfig: config.SDKConfig{ForceModelPrefix: tt.force}, OpenAICompatibility: []config.OpenAICompatibility{{Name: "test", Prefix: tt.prefix, Models: tt.models}}})
			if got := m.ConfiguredAliasUpstreamModels(tt.request); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDestinationConfiguredAliasNilSafety(t *testing.T) {
	for _, m := range []*Manager{nil, NewManager(nil, nil, nil)} {
		if got := m.ConfiguredAliasUpstreamModels("alias"); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	}
}

func TestDestinationOAuthAliasMatchesExecution(t *testing.T) {
	m := NewManager(nil, nil, nil)
	cfg := &config.Config{SDKConfig: config.SDKConfig{ForceModelPrefix: true}, OAuthModelAlias: map[string][]config.OAuthModelAlias{"codex": {{Name: "gpt-test", Alias: "alias"}, {Name: "claude-test", Alias: "alias"}}}}
	m.SetConfig(cfg)
	m.SetOAuthModelAlias(cfg.OAuthModelAlias)
	a := &Auth{ID: "fixture", Provider: "codex", Prefix: "team"}
	m.auths[a.ID] = a
	if got := m.ConfiguredAliasUpstreamModels("team/alias(high)"); !reflect.DeepEqual(got, []string{"gpt-test(high)"}) {
		t.Fatalf("got %v", got)
	}
	if got := m.ConfiguredAliasUpstreamModels("alias"); len(got) != 0 {
		t.Fatalf("unprefixed alias got %v", got)
	}
	SetOAuthModelAliasesAttribute(a, []config.OAuthModelAlias{{Name: "claude-per-auth", Alias: "alias"}})
	if got := m.ConfiguredAliasUpstreamModels("team/alias"); !reflect.DeepEqual(got, []string{"claude-per-auth"}) {
		t.Fatalf("per-auth alias got %v", got)
	}
}

func TestDestinationAPIKeyAliasCompiledFallback(t *testing.T) {
	for _, tt := range []struct {
		name, request string
		models        []config.CodexModel
		want          []string
	}{
		{name: "first match", request: "alias", models: []config.CodexModel{{Name: "gpt-test", Alias: "alias"}, {Name: "claude-test", Alias: "alias"}}, want: []string{"gpt-test"}},
		{name: "compiled base fallback", request: "alias", models: []config.CodexModel{{Name: "claude-test", Alias: "alias(high)"}}, want: []string{"claude-test"}},
		{name: "compiled full suffix", request: "alias(high)", models: []config.CodexModel{{Name: "gpt-test", Alias: "alias"}, {Name: "claude-test", Alias: "alias(high)"}}, want: []string{"claude-test(high)"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.SetConfig(&config.Config{CodexKey: []config.CodexKey{{Models: tt.models}}})
			if got := m.ConfiguredAliasUpstreamModels(tt.request); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
