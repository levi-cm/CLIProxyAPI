package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type policyTestSource struct {
	mu        sync.Mutex
	settings  accountpolicy.Settings
	snapshots map[string]accountpolicy.Snapshot
	decisions []accountpolicy.Decision
}

func (p *policyTestSource) Settings() accountpolicy.Settings {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.settings
}
func (p *policyTestSource) Snapshot(id string) (accountpolicy.Snapshot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.snapshots[id]
	return s, ok
}
func (p *policyTestSource) RecordDecision(d accountpolicy.Decision) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.decisions = append(p.decisions, d)
}

func deadlineFixture(t *testing.T) (*EarliestDeadlineSelector, *policyTestSource, []*Auth, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	settings := accountpolicy.DefaultSettings()
	settings.Enabled = true
	settings.Automation = "auto_expiring"
	p := &policyTestSource{settings: settings, snapshots: map[string]accountpolicy.Snapshot{}}
	for _, id := range []string{"a", "b"} {
		days := 4
		if id == "b" {
			days = 7
		}
		p.snapshots[id] = accountpolicy.Snapshot{Identity: accountpolicy.Identity{CredentialID: id, AccountID: "account-" + id, WorkspaceID: "workspace-" + id, Provider: "codex"}, Eligible: true, Status: "healthy", ObservedAt: now, InventoryObservedAt: now, InventoryComplete: true, Buckets: []accountpolicy.Bucket{{Scope: "ordinary", DurationSeconds: 604800, UsedPercent: 20, ObservedAt: now, ResetAt: now.Add(time.Duration(days) * 24 * time.Hour)}}}
	}
	s := NewEarliestDeadlineSelector(p, &FillFirstSelector{})
	s.now = func() time.Time { return now }
	s.SetIdleCheck(func(string) bool { return true })
	t.Cleanup(s.Stop)
	return s, p, []*Auth{{ID: "a", Provider: "codex", Attributes: map[string]string{"priority": "10"}}, {ID: "b", Provider: "codex"}}, now
}
func addExpiringCredit(p *policyTestSource, now time.Time) {
	b := p.snapshots["b"]
	expiry := now.Add(24 * time.Hour)
	b.Credits = []accountpolicy.Credit{{ID: "credit-b", Type: "codex_rate_limits", Status: "available", DetailsKnown: true, Scopes: []string{"ordinary"}, ExpiresAt: &expiry}}
	p.snapshots["b"] = b
}

// Static priority cannot erase the account whose saved credit expires tomorrow.
func TestDeadlineSelectorRanksAcrossStaticPriorities(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	addExpiringCredit(p, now)
	got, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("got %q, want expiring B", got.ID)
	}
	if len(p.decisions) != 1 || p.decisions[0].Reason != "expiring_reset_credit" {
		t.Fatalf("decisions=%+v", p.decisions)
	}
}

func TestDeadlineSelectorUnknownFallbackPreservesExclusions(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	addExpiringCredit(p, now)
	a := p.snapshots["a"]
	a.ObservedAt = now.Add(-3 * time.Minute)
	p.snapshots["a"] = a
	got, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "a" {
		t.Fatalf("fallback got %q, want static A", got.ID)
	}
	p.settings.Accounts["a"] = accountpolicy.AccountControl{Hold: true}
	got, err = s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("held stale A remained eligible: %q", got.ID)
	}
	p.settings.ForceAccount = "a"
	if _, err = s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths); err == nil {
		t.Fatal("force bypassed held account")
	}
}

func TestDeadlineAffinityMigrationRequiresReplayableBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, body              string
		websocket, busy, strict bool
		want                    string
	}{
		{"replayable", `{"input":[{"role":"user","content":"hello"}]}`, false, false, false, "b"},
		{"previous_response", `{"previous_response_id":"resp-a","input":"continue"}`, false, false, false, "a"},
		{"encrypted_reasoning", `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, false, false, false, "a"},
		{"turn_state", `{"turn_state":"opaque","input":"continue"}`, false, false, false, "a"},
		{"unknown_shape", `{"opaque":"continue"}`, false, false, false, "a"},
		{"live_websocket", `{"input":"hello"}`, true, false, false, "a"},
		{"active_request", `{"input":"hello"}`, false, true, false, "a"},
		{"strict", `{"input":"hello"}`, false, false, true, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p, auths, now := deadlineFixture(t)
			opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{"thread"}}, OriginalRequest: []byte(`{"input":"hello"}`), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "client-1"}}
			ctx := context.Background()
			got, err := s.Pick(ctx, "codex", "gpt-5", opts, auths)
			if err != nil || got.ID != "a" {
				t.Fatalf("initial=%v err=%v", got, err)
			}
			addExpiringCredit(p, now)
			if tc.strict {
				p.settings.Affinity = "strict"
			}
			if tc.busy {
				s.SetIdleCheck(func(string) bool { return false })
			}
			if tc.websocket {
				ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
			}
			opts.OriginalRequest = []byte(tc.body)
			got, err = s.Pick(ctx, "codex", "gpt-5", opts, auths)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != tc.want {
				t.Fatalf("got %q want %q", got.ID, tc.want)
			}
		})
	}
}

func TestDeadlineAffinityIsolatesClientsAndUnsafeUnavailable(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{"same"}}, OriginalRequest: []byte(`{"input":"hello"}`), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "one"}}
	if _, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths); err != nil {
		t.Fatal(err)
	}
	addExpiringCredit(p, now)
	opts.Metadata = map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "two"}
	got, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || got.ID != "b" {
		t.Fatalf("second client=%v err=%v", got, err)
	}
	opts.Metadata = map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "one"}
	opts.OriginalRequest = []byte(`{"previous_response_id":"resp-a","input":"continue"}`)
	p.settings.Accounts["a"] = accountpolicy.AccountControl{Hold: true}
	if _, err = s.Pick(context.Background(), "codex", "gpt-5", opts, auths); err == nil {
		t.Fatal("unsafe unavailable continuation changed account")
	}
}

func TestDeadlineAffinityRejectsUnknownOwnerContinuation(t *testing.T) {
	for _, body := range []string{`{"previous_response_id":"resp-owned-elsewhere","input":"continue"}`, `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, `{"turn_state":"opaque","input":"continue"}`} {
		t.Run(body, func(t *testing.T) {
			s, _, auths, _ := deadlineFixture(t)
			opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{"new"}}, OriginalRequest: []byte(body), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "client-1"}}
			if _, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths); err == nil {
				t.Fatal("unknown account owner accepted account-specific continuation")
			}
		})
	}
}

func TestDeadlineAffinityAllowsNewWebsocketWithFullInput(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	addExpiringCredit(p, now)
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{"new"}}, OriginalRequest: []byte(`{"input":"hello"}`), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "client-1"}}
	got, err := s.Pick(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), "codex", "gpt-5", opts, auths)
	if err != nil || got.ID != "b" {
		t.Fatalf("new websocket selected=%v err=%v", got, err)
	}
}

func TestDeadlineSelectorAppliesModelSpecificQuotaAfterAliasResolution(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	addExpiringCredit(p, now)
	b := p.snapshots["b"]
	blocked := false
	b.Buckets = append(b.Buckets, accountpolicy.Bucket{Scope: "ordinary", Model: "upstream-model", DurationSeconds: 18000, Allowed: &blocked, UsedPercent: 100, ObservedAt: now})
	p.snapshots["b"] = b
	s.SetModelResolver(func(*Auth, string) string { return "upstream-model" })
	got, err := s.Pick(context.Background(), "codex", "friendly-alias", cliproxyexecutor.Options{}, auths)
	if err != nil || got.ID != "a" {
		t.Fatalf("model-blocked alias picked=%v err=%v", got, err)
	}
}

func TestDeadlineFallbackSettingsChangeTakesEffect(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	p.settings.Fallback = "fill-first"
	a := p.snapshots["a"]
	a.ObservedAt = now.Add(-time.Hour)
	p.snapshots["a"] = a
	for _, candidate := range auths {
		candidate.Attributes = nil
	}
	if _, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths); err != nil {
		t.Fatal(err)
	}
	p.settings.Fallback = "round-robin"
	first, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("dynamic fallback settings remained fill-first")
	}
}

func TestDeadlineLCPAffinityMovesOnlyAtSafeBoundary(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: []byte(`{"messages":[{"role":"system","content":"stable"},{"role":"user","content":"first"}]}`), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller"}}
	initial, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || initial.ID != "a" {
		t.Fatalf("initial=%v err=%v", initial, err)
	}
	addExpiringCredit(p, now)
	next, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || next.ID != "b" {
		t.Fatalf("safe LCP continuation=%v err=%v", next, err)
	}
}
