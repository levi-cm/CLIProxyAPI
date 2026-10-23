package helps

import "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"

// mergeReportedUsage carries only fields missing from a later partial snapshot.
// Explicitly reported zeros replace earlier values. Recompute the canonical
// breakdown using the parser's original protocol semantics after merging.
func mergeReportedUsage(old, update usage.Detail) usage.Detail {
	m := update
	if m.ResponseServiceTier == "" {
		m.ResponseServiceTier = old.ResponseServiceTier
	}
	if !update.Measurements.Input {
		m.InputTokens = old.InputTokens
	}
	if !update.Measurements.Output {
		m.OutputTokens = old.OutputTokens
	}
	if !update.Measurements.CacheRead {
		m.CacheReadTokens = old.CacheReadTokens
		m.CachedTokens = old.CachedTokens
	}
	if !update.Measurements.CacheWrite {
		m.CacheCreationTokens = old.CacheCreationTokens
	}
	if !update.Measurements.Reasoning {
		m.ReasoningTokens = old.ReasoningTokens
	}
	if !update.Measurements.CacheWrite5m {
		m.CacheWrite5mTokens = old.CacheWrite5mTokens
	}
	if !update.Measurements.CacheWrite1h {
		m.CacheWrite1hTokens = old.CacheWrite1hTokens
	}
	m.Measurements = mergePresence(old.Measurements, update.Measurements)
	if m.MeasurementSemantics == "" {
		m.MeasurementSemantics = old.MeasurementSemantics
	}
	if !update.Measurements.Total {
		if old.Measurements.Total {
			m.TotalTokens = old.TotalTokens
		} else {
			m.TotalTokens = 0
		}
	}
	// An earlier complete cumulative total cannot describe newly reported output.
	// Preserve inconsistent or larger reported totals rather than inventing data.
	if !update.Measurements.Total && old.Measurements.Total && old.TokenBreakdown.Quality == usage.TokenAccountingQualityComplete {
		parts := []int64{m.InputTokens, m.OutputTokens}
		switch m.MeasurementSemantics {
		case "independent":
			parts = append(parts, m.CacheReadTokens, m.CacheCreationTokens)
		case "separate_reasoning":
			parts = append(parts, m.ReasoningTokens)
		}
		if total, ok := safeUsageTokenSum(parts...); ok && total > m.TotalTokens {
			m.TotalTokens = 0
			m.Measurements.Total = false
		}
	}
	switch m.MeasurementSemantics {
	case "subset":
		m.TokenBreakdown = usage.NewSubsetTokenBreakdown(m.InputTokens, m.CacheReadTokens, m.CacheCreationTokens, m.OutputTokens, m.ReasoningTokens, m.TotalTokens)
	case "independent":
		nonReasoning := m.OutputTokens - m.ReasoningTokens
		if nonReasoning < 0 {
			total, ok := safeUsageTokenSum(m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.CacheCreationTokens)
			if !ok {
				total = m.TotalTokens
			}
			m.TotalTokens = total
			m.TokenBreakdown = invalidUsageTokenBreakdown(total)
			return m
		}
		m.TokenBreakdown = usage.NewIndependentTokenBreakdown(m.InputTokens, m.CacheReadTokens, m.CacheCreationTokens, nonReasoning, m.ReasoningTokens, m.TotalTokens)
	case "separate_reasoning":
		m.TokenBreakdown = usage.NewSeparateReasoningTokenBreakdown(m.InputTokens, m.CacheReadTokens, m.CacheCreationTokens, m.OutputTokens, m.ReasoningTokens, m.TotalTokens)
	default:
		m.TokenBreakdown = usage.NewUnclassifiedTokenBreakdown(m.TotalTokens)
	}
	m.TotalTokens = m.TokenBreakdown.TotalTokens
	return m
}

func hasReportedMeasurements(p usage.MeasurementPresence) bool {
	return p.Input || p.Output || p.CacheRead || p.CacheWrite || p.Reasoning || p.Total || p.CacheWrite5m || p.CacheWrite1h
}
