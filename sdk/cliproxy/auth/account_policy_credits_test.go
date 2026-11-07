package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// exhaustCreditAccount turns account a into a weekly-exhausted Pro account with
// the given provider credit balance.
func exhaustCreditAccount(p *policyTestSource, now time.Time, balance float64) {
	a := p.snapshots["a"]
	blocked := false
	a.Status = "quota_blocked"
	a.Eligible = false
	a.Buckets = []accountpolicy.Bucket{{Scope: "ordinary", DurationSeconds: 604800, UsedPercent: 100, Allowed: &blocked, ObservedAt: now, ResetAt: now.Add(4 * 24 * time.Hour)}}
	a.CreditBalance = &balance
	p.snapshots["a"] = a
}

func enableCreditSpending(p *policyTestSource) {
	p.settings.CreditSpending = accountpolicy.CreditSpending{Enabled: true}
}

func TestDeadlineSelectorSpendsCreditsBeforeSwitchingAccounts(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	exhaustCreditAccount(p, now, 62454.42)

	// Without credit spending the exhausted account is skipped.
	got, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil || got.ID != "b" {
		t.Fatalf("disabled credit spending picked %v err=%v, want b", got, err)
	}

	enableCreditSpending(p)
	got, err = s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil || got.ID != "a" {
		t.Fatalf("credit-first picked %v err=%v, want exhausted account a", got, err)
	}
	if last := p.decisions[len(p.decisions)-1]; last.Reason != "credit_balance" || last.CredentialID != "a" {
		t.Fatalf("decision = %+v", last)
	}
}

func TestDeadlineSelectorStopsCreditSpendingAtFloor(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	enableCreditSpending(p)
	exhaustCreditAccount(p, now, 7600) // below the 7,500 floor plus 250 margin
	got, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil || got.ID != "b" {
		t.Fatalf("picked %v err=%v, want b once a reaches its floor", got, err)
	}
	// With both accounts at their floors nothing is eligible.
	b := p.snapshots["b"]
	blocked := false
	b.Status, b.Eligible = "quota_blocked", false
	b.Buckets = []accountpolicy.Bucket{{Scope: "ordinary", DurationSeconds: 604800, UsedPercent: 100, Allowed: &blocked, ObservedAt: now, ResetAt: now.Add(24 * time.Hour)}}
	low := 7000.0
	b.CreditBalance = &low
	p.snapshots["b"] = b
	if got, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths); err == nil {
		t.Fatalf("picked %v below both floors", got.ID)
	}
	// A manual unlock makes the reserve usable again.
	until := now.Add(time.Hour)
	p.settings.Accounts = map[string]accountpolicy.AccountControl{"b": {CreditReserveUnlockedUntil: &until}}
	if got, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths); err != nil || got.ID != "b" {
		t.Fatalf("unlock picked %v err=%v, want b", got, err)
	}
}

func TestDeadlineSelectorPrefersAllowanceThatWouldBeLost(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	enableCreditSpending(p)
	exhaustCreditAccount(p, now, 62454.42)
	b := p.snapshots["b"]
	b.Buckets[0].ResetAt = now.Add(20 * time.Hour) // inside the 36h guard
	p.snapshots["b"] = b
	got, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil || got.ID != "b" {
		t.Fatalf("picked %v err=%v, want b whose allowance renews within the guard", got, err)
	}
}

func TestDeadlineSelectorIgnoresStaleCreditBalance(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	enableCreditSpending(p)
	exhaustCreditAccount(p, now, 62454.42)
	a := p.snapshots["a"]
	a.ObservedAt = now.Add(-15 * time.Minute)
	p.snapshots["a"] = a
	got, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil || got.ID != "b" {
		t.Fatalf("picked %v err=%v, want b when a's balance is stale", got, err)
	}
}

func creditSessionOptions() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"credit-session"}}, OriginalRequest: []byte(`{"input":"full replayable input"}`), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller"}}
}

func TestDeadlineSessionMigratesToCreditAccountAtBoundary(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	exhaustCreditAccount(p, now, 62454.42)
	first, err := s.Pick(context.Background(), "codex", "gpt-5", creditSessionOptions(), auths)
	if err != nil || first.ID != "b" {
		t.Fatalf("first=%v err=%v, want allowance account b", first, err)
	}
	enableCreditSpending(p)
	next, err := s.Pick(context.Background(), "codex", "gpt-5", creditSessionOptions(), auths)
	if err != nil || next.ID != "a" {
		t.Fatalf("next=%v err=%v, want migration to credit account a", next, err)
	}
	if last := p.decisions[len(p.decisions)-1]; last.Reason != "deadline_migration_from_b" {
		t.Fatalf("decision=%+v", last)
	}
}

func TestDeadlineCreditSessionStaysUntilGuardOrFloor(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	enableCreditSpending(p)
	exhaustCreditAccount(p, now, 62454.42)
	first, err := s.Pick(context.Background(), "codex", "gpt-5", creditSessionOptions(), auths)
	if err != nil || first.ID != "a" {
		t.Fatalf("first=%v err=%v, want credit account a", first, err)
	}
	again, err := s.Pick(context.Background(), "codex", "gpt-5", creditSessionOptions(), auths)
	if err != nil || again.ID != "a" {
		t.Fatalf("again=%v err=%v, want retained credit account", again, err)
	}
	// Allowance renewing inside the guard pulls the session over at a boundary.
	b := p.snapshots["b"]
	b.Buckets[0].ResetAt = now.Add(12 * time.Hour)
	p.snapshots["b"] = b
	moved, err := s.Pick(context.Background(), "codex", "gpt-5", creditSessionOptions(), auths)
	if err != nil || moved.ID != "b" {
		t.Fatalf("moved=%v err=%v, want b whose allowance would otherwise be lost", moved, err)
	}
}

func TestDeadlineNonReplayableCreditSessionKeepsOwnerAtFloor(t *testing.T) {
	s, p, auths, now := deadlineFixture(t)
	enableCreditSpending(p)
	exhaustCreditAccount(p, now, 62454.42)
	opts := creditSessionOptions()
	if got, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths); err != nil || got.ID != "a" {
		t.Fatalf("got=%v err=%v", got, err)
	}
	exhaustCreditAccount(p, now, 7000)
	opts.OriginalRequest = []byte(`{"previous_response_id":"resp-on-a","input":"continue"}`)
	if got, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths); err == nil {
		t.Fatalf("account-bound continuation moved to %s after the floor was reached", got.ID)
	}
}
