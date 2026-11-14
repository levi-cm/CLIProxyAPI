package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyledger"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/subagentadvice"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// The background sampler reads the request ledger over the whole trend
// window and reports how much of it the ledger covers; keyless requests only
// reuse that reading.
func TestSubagentInventoryReadsLedgerTrendWindow(t *testing.T) {
	fx := newAdviceFixture(t)
	a, err := accountpolicyledger.New(t.TempDir(), func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	record := func(at time.Time, output int64) {
		a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), Provider: "claude", AuthID: "claude-a", Model: "claude-opus-5-5", RequestedAt: at,
			Detail: usage.Detail{InputTokens: 1000, OutputTokens: output, TotalTokens: 1000 + output, Measurements: usage.MeasurementPresence{Input: true, Output: true}}})
	}
	record(now.Add(-50*time.Hour), 500)
	record(now.Add(-10*time.Hour), 700)

	source := newSubagentInventory(fx.server.handlers.AuthManager, nil, func() *accountpolicyledger.Archive { return a })
	source.now = func() time.Time { return now }
	read := func(span time.Duration) map[string]subagentadvice.Account {
		inv, errInv := source.Inventory(subagentadvice.WithTrendSpan(subagentadvice.WithBackgroundRefresh(context.Background()), span))
		if errInv != nil {
			t.Fatal(errInv)
		}
		out := map[string]subagentadvice.Account{}
		for _, acct := range inv.Accounts {
			out[acct.ID] = acct
		}
		return out
	}

	// No request predates a 72h window: coverage starts at the first request.
	byID := read(72 * time.Hour)
	ua, ub := byID["claude-a"].Usage, byID["claude-b"].Usage
	if ua == nil || !ua.TrendFrom.Equal(now.Add(-50*time.Hour)) || !ua.TrendTo.Equal(now) || len(ua.Trend) != 1 || ua.Trend[0].OutputTokens != 1200 {
		t.Fatalf("claude-a usage = %+v", ua)
	}
	// An account without requests still gets the covered window: zero use.
	if ub == nil || len(ub.Trend) != 0 || !ub.TrendFrom.Equal(ua.TrendFrom) {
		t.Fatalf("claude-b usage = %+v", ub)
	}

	// A 24h window is fully covered (a request predates it) and holds only
	// the recent request; a changed span is read again at once.
	byID = read(24 * time.Hour)
	if ua = byID["claude-a"].Usage; !ua.TrendFrom.Equal(now.Add(-24*time.Hour)) || ua.Trend[0].OutputTokens != 700 {
		t.Fatalf("24h usage = %+v", ua)
	}

	// Keyless requests reuse the sampler's reading and never touch the archive.
	inv, err := source.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range inv.Accounts {
		if acct.ID == "claude-a" && (acct.Usage == nil || acct.Usage.Trend[0].OutputTokens != 700) {
			t.Fatalf("keyless usage = %+v", acct.Usage)
		}
	}
}

// Dense allowance observations are thinned to one per few minutes without
// losing the latest reading or a window change, and the background sampler
// reads the whole trend window.
func TestSubagentAllowanceHistoryIsThinned(t *testing.T) {
	a, err := accountpolicyledger.New(t.TempDir(), func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	oldReset, newReset := now.Add(-time.Hour), now.Add(167*time.Hour)
	observe := func(at, reset time.Time, used float64) {
		if errObserve := a.Observe(accountpolicyledger.Observation{CredentialID: "codex-a", Provider: "codex", DurationSeconds: 604800, ObservedAt: at, UsedPercent: used, ResetAt: reset}); errObserve != nil {
			t.Fatal(errObserve)
		}
	}
	observe(now.Add(-50*time.Hour), oldReset, 10) // inside a 72h window only
	start := now.Add(-2 * time.Hour)
	for i := 0; i < 120; i++ { // every 30s for an hour
		observe(start.Add(time.Duration(i)*30*time.Second), oldReset, 20+float64(i)/10)
	}
	observe(now.Add(-30*time.Minute), newReset, 1)
	observe(now.Add(-29*time.Minute), newReset, 2)

	source := newSubagentInventory(nil, nil, func() *accountpolicyledger.Archive { return a })
	ctx := subagentadvice.WithTrendSpan(subagentadvice.WithBackgroundRefresh(context.Background()), 72*time.Hour)
	samples := source.allowanceHistory(ctx, "codex-a", now)
	if len(samples) < 15 || len(samples) > 25 { // ~one per 4 min of 122 observations
		t.Fatalf("samples = %d", len(samples))
	}
	if !samples[0].At.Equal(now.Add(-50*time.Hour)) || samples[len(samples)-1].UsedPercent != 2 {
		t.Fatalf("first %+v last %+v", samples[0], samples[len(samples)-1])
	}
	lastOld := 0.0
	for _, s := range samples {
		if s.ResetAt.Equal(oldReset) {
			lastOld = s.UsedPercent
		}
	}
	if lastOld != 20+11.9 {
		t.Fatalf("latest reading of the old window lost: %v", lastOld)
	}
	// Keyless reads get the cached slice; a 24h-only read drops the old sample.
	if got := source.allowanceHistory(context.Background(), "codex-a", now); len(got) != len(samples) {
		t.Fatalf("cached = %d", len(got))
	}
	short := source.allowanceHistory(subagentadvice.WithBackgroundRefresh(context.Background()), "codex-a", now)
	if short[0].At.Before(now.Add(-24 * time.Hour)) {
		t.Fatalf("24h read starts at %v", short[0].At)
	}
}
