package auth

import "strings"

// ConfiguredAliasUpstreamModels returns every configured upstream model name that the given
// client-facing alias can resolve to, across API-key, OpenAI-compatibility and OAuth aliases.
// Alias pools can map one alias to several upstreams, while the registry keeps only one entry's
// metadata, so destination checks must inspect the whole configuration.
func (m *Manager) ConfiguredAliasUpstreamModels(alias string) []string {
	alias = strings.TrimSpace(alias)
	cfg := m.runtimeConfigSnapshot()
	if alias == "" || cfg == nil {
		return nil
	}
	var upstreams []string
	add := func(entries []modelAliasEntry) {
		for _, entry := range entries {
			if entry != nil && strings.EqualFold(strings.TrimSpace(entry.GetAlias()), alias) {
				upstreams = append(upstreams, strings.TrimSpace(entry.GetName()))
			}
		}
	}
	for i := range cfg.ClaudeKey {
		add(asModelAliasEntries(cfg.ClaudeKey[i].Models))
	}
	for i := range cfg.CodexKey {
		add(asModelAliasEntries(cfg.CodexKey[i].Models))
	}
	for i := range cfg.GeminiKey {
		add(asModelAliasEntries(cfg.GeminiKey[i].Models))
	}
	for i := range cfg.InteractionsKey {
		add(asModelAliasEntries(cfg.InteractionsKey[i].Models))
	}
	for i := range cfg.XAIKey {
		add(asModelAliasEntries(cfg.XAIKey[i].Models))
	}
	for i := range cfg.MetaKey {
		add(asModelAliasEntries(cfg.MetaKey[i].Models))
	}
	for i := range cfg.VertexCompatAPIKey {
		add(asModelAliasEntries(cfg.VertexCompatAPIKey[i].Models))
	}
	for i := range cfg.OpenAICompatibility {
		add(asModelAliasEntries(cfg.OpenAICompatibility[i].Models))
	}
	for _, entries := range cfg.OAuthModelAlias {
		for _, entry := range entries {
			if strings.EqualFold(strings.TrimSpace(entry.Alias), alias) {
				upstreams = append(upstreams, strings.TrimSpace(entry.Name))
			}
		}
	}
	return upstreams
}
