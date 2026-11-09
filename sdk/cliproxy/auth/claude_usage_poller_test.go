package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func claudeUsageTestHeaders() http.Header {
	h := http.Header{}
	h.Set(claudeWeeklyUtilizationSignal, "0.55")
	h.Set(claudeWeeklyResetSignal, "1794754800")
	return h
}

func registerClaudeUsageAuth(t *testing.T, m *Manager, id string, mutate func(*Auth)) {
	t.Helper()
	a := &Auth{ID: id, Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "tok-" + id}}
	if mutate != nil {
		mutate(a)
	}
	if _, errRegister := m.Register(context.Background(), a); errRegister != nil {
		t.Fatalf("register %s: %v", id, errRegister)
	}
}

func TestClaudeUsagePollRefreshesOnlyIdleClaudeAccounts(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()
	registerClaudeUsageAuth(t, m, "idle", func(a *Auth) { a.Quota.ObservedAt = now.Add(-20 * time.Minute) })
	registerClaudeUsageAuth(t, m, "never", nil)
	registerClaudeUsageAuth(t, m, "busy", func(a *Auth) { a.Quota.ObservedAt = now.Add(-time.Minute) })
	registerClaudeUsageAuth(t, m, "off", func(a *Auth) { a.Disabled = true; a.Status = StatusDisabled })
	registerClaudeUsageAuth(t, m, "notoken", func(a *Auth) { a.Metadata = map[string]any{} })
	if _, errRegister := m.Register(context.Background(), &Auth{ID: "codex", Provider: "codex", Metadata: map[string]any{"access_token": "t"}}); errRegister != nil {
		t.Fatal(errRegister)
	}

	fetched := map[string]string{}
	m.pollClaudeUsage(context.Background(), func(_ context.Context, a *Auth, token string) (http.Header, error) {
		fetched[a.ID] = token
		return claudeUsageTestHeaders(), nil
	})

	if len(fetched) != 2 || fetched["idle"] != "tok-idle" || fetched["never"] != "tok-never" {
		t.Fatalf("fetched = %v, want only idle and never with their own tokens", fetched)
	}
	for _, id := range []string{"idle", "never"} {
		a, _ := m.GetByID(id)
		if a.Quota.Signals[claudeWeeklyUtilizationSignal] != "0.55" || a.Quota.Signals[claudeWeeklyResetSignal] != "1794754800" || !a.Quota.ObservedAt.After(now) {
			t.Fatalf("%s observation not refreshed: %+v", id, a.Quota)
		}
		if a.Quota.Exceeded || a.Unavailable {
			t.Fatalf("%s cooldown state changed: %+v", id, a.Quota)
		}
	}
}

func TestClaudeUsagePollFailureKeepsObservation(t *testing.T) {
	m := NewManager(nil, nil, nil)
	old := time.Now().Add(-time.Hour)
	registerClaudeUsageAuth(t, m, "a", func(a *Auth) {
		a.Quota.ObservedAt = old
		a.Quota.Signals = map[string]string{claudeWeeklyUtilizationSignal: "0.1"}
	})
	m.pollClaudeUsage(context.Background(), func(context.Context, *Auth, string) (http.Header, error) {
		return nil, errors.New("status 429")
	})
	m.pollClaudeUsage(context.Background(), func(context.Context, *Auth, string) (http.Header, error) {
		return http.Header{}, nil
	})
	a, _ := m.GetByID("a")
	if !a.Quota.ObservedAt.Equal(old) || a.Quota.Signals[claudeWeeklyUtilizationSignal] != "0.1" {
		t.Fatalf("failed or empty poll changed observation: %+v", a.Quota)
	}
}

func TestClaudeUsagePollNeverOverwritesNewerResponse(t *testing.T) {
	m := NewManager(nil, nil, nil)
	registerClaudeUsageAuth(t, m, "a", nil)
	started := time.Now()
	m.mu.Lock()
	m.auths["a"].Quota.ObservedAt = started.Add(time.Second)
	m.auths["a"].Quota.Signals = map[string]string{claudeWeeklyUtilizationSignal: "0.2"}
	m.mu.Unlock()
	a, _ := m.GetByID("a")
	if m.observeClaudeUsage("a", a.RegistrationEpoch, claudeUsageTestHeaders(), started) {
		t.Fatal("poll overwrote an observation recorded after it started")
	}
	if m.observeClaudeUsage("a", a.RegistrationEpoch+1, claudeUsageTestHeaders(), started.Add(time.Hour)) {
		t.Fatal("poll applied to a re-registered auth")
	}
	if got, _ := m.GetByID("a"); got.Quota.Signals[claudeWeeklyUtilizationSignal] != "0.2" {
		t.Fatalf("observation changed: %+v", got.Quota)
	}
}

func TestClaudeUsagePollFeedsDeadlineRanking(t *testing.T) {
	m := NewManager(nil, nil, nil)
	registerClaudeUsageAuth(t, m, "a", nil)
	m.pollClaudeUsage(context.Background(), func(context.Context, *Auth, string) (http.Header, error) {
		return claudeUsageTestHeaders(), nil
	})
	a, _ := m.GetByID("a")
	s := &EarliestDeadlineSelector{}
	deadline, ok := s.claudeDeadline(a, time.Unix(1794700000, 0))
	if !ok || deadline.Unix() != 1794754800 {
		t.Fatalf("deadline = %v %v, want polled weekly reset", deadline, ok)
	}
}
