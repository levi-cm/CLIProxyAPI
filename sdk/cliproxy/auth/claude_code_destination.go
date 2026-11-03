package auth

import "strings"

// ConfiguredAliasUpstreamModels returns every configured upstream model name that the given
// client-facing alias can resolve to, across API-key, OpenAI-compatibility and OAuth aliases.
// Compatibility pools can map one alias to several upstreams, while the registry keeps only one entry's
// metadata, so destination checks must inspect the whole configuration.
func (m *Manager) ConfiguredAliasUpstreamModels(alias string) []string {
	alias = strings.TrimSpace(alias)
	cfg := m.runtimeConfigSnapshot()
	if alias == "" || cfg == nil {
		return nil
	}
	var upstreams []string
	add := func(prefix string, entries []modelAliasEntry, pool bool) {
		requested, ok := destinationModelForPrefix(alias, prefix, cfg.ForceModelPrefix)
		if ok {
			if pool {
				upstreams = append(upstreams, resolveModelAliasPoolFromConfigModels(requested, entries)...)
			} else if upstream := destinationAPIKeyAliasModel(requested, entries); upstream != "" {
				upstreams = append(upstreams, upstream)
			}
		}
	}
	for i := range cfg.ClaudeKey {
		add(cfg.ClaudeKey[i].Prefix, asModelAliasEntries(cfg.ClaudeKey[i].Models), false)
	}
	for i := range cfg.CodexKey {
		add(cfg.CodexKey[i].Prefix, asModelAliasEntries(cfg.CodexKey[i].Models), false)
	}
	for i := range cfg.GeminiKey {
		add(cfg.GeminiKey[i].Prefix, asModelAliasEntries(cfg.GeminiKey[i].Models), false)
	}
	for i := range cfg.InteractionsKey {
		add(cfg.InteractionsKey[i].Prefix, asModelAliasEntries(cfg.InteractionsKey[i].Models), false)
	}
	for i := range cfg.XAIKey {
		add(cfg.XAIKey[i].Prefix, asModelAliasEntries(cfg.XAIKey[i].Models), false)
	}
	for i := range cfg.MetaKey {
		add(cfg.MetaKey[i].Prefix, asModelAliasEntries(cfg.MetaKey[i].Models), false)
	}
	for i := range cfg.VertexCompatAPIKey {
		add(cfg.VertexCompatAPIKey[i].Prefix, asModelAliasEntries(cfg.VertexCompatAPIKey[i].Models), false)
	}
	for i := range cfg.OpenAICompatibility {
		if cfg.OpenAICompatibility[i].Disabled {
			continue
		}
		add(cfg.OpenAICompatibility[i].Prefix, asModelAliasEntries(cfg.OpenAICompatibility[i].Models), true)
	}
	// OAuth aliases are first-match (not pools), may be per-account, and inherit the
	// credential prefix. Use the same resolver as execution rather than merging channels.
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, credential := range m.auths {
		if credential == nil || credential.Disabled || credential.Status == StatusDisabled || isConfiguredModelRoutingAuth(credential) || modelAliasChannel(credential) == "" {
			continue
		}
		requested, ok := destinationModelForPrefix(alias, credential.Prefix, cfg.ForceModelPrefix)
		if !ok {
			continue
		}
		if result := m.resolveOAuthModelAliasWithResult(credential, requested); result.UpstreamModel != "" {
			upstreams = append(upstreams, result.UpstreamModel)
		}
	}
	return upstreams
}

// Match the case-sensitive prefix stripping used by rewriteModelForAuth and the
// unprefixed publication exception in applyModelPrefixes.
func destinationModelForPrefix(model, prefix string, force bool) (string, bool) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return model, true
	}
	if rest, ok := strings.CutPrefix(model, prefix+"/"); ok {
		return rest, true
	}
	parsed, _ := modelAliasLookupCandidates(model)
	return model, !force || prefix == model || prefix == parsed.ModelName
}

// API-key routes compile exact aliases and first-entry suffix-stripped fallbacks.
// Unlike compatibility pools, duplicate aliases within a key use only the first entry.
func destinationAPIKeyAliasModel(requested string, entries []modelAliasEntry) string {
	aliases := make(map[string]string)
	compileAPIKeyModelAliasForModels(aliases, entries)
	parsed, candidates := modelAliasLookupCandidates(requested)
	for _, candidate := range candidates {
		if upstream := aliases[strings.ToLower(candidate)]; upstream != "" {
			return preserveResolvedModelSuffix(upstream, parsed)
		}
	}
	return resolveModelAliasFromConfigModels(requested, entries)
}
