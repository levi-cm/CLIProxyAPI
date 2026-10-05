package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Falling back to global last-selection data would leak another client's account.
func TestPolicyClientRuntimeIsolatesCallerSessionAndFailedAttempts(t *testing.T) {
	m := NewManager(nil, nil, nil)
	reader, ok := any(m).(interface {
		PolicyClientRuntime(string, string, time.Time) (map[string]int, string, time.Time)
	})
	if !ok {
		t.Fatal("session-scoped client usage is unavailable")
	}
	for _, id := range []string{"a", "b"} {
		if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex"}); err != nil {
			t.Fatal(err)
		}
	}
	start := func(scope, sessionID, id string) (cliproxyexecutor.Options, *policyRequestLease) {
		headers := make(http.Header)
		headers.Set("Session_id", sessionID)
		headers.Set("X-Codex-Turn-Metadata", `{"agent_name":"reviewer"}`)
		headers.Set("X-Openai-Subagent", "true")
		opts, lease := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{Headers: headers, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: scope}}, policyRequestDemand{})
		a, _ := m.GetByID(id)
		publishSelectedAuthMetadata(opts.Metadata, a)
		return opts, lease
	}
	optsA, leaseA := start("caller-a", "thread-one", "a")
	_, leaseB := start("caller-b", "thread-one", "b")
	defer leaseA.release()
	defer leaseB.release()
	active, last, _ := reader.PolicyClientRuntime("caller-a", "codex:thread-one", time.Now())
	if len(active) != 1 || active["a"] != 1 || last != "" {
		t.Fatalf("incorrect caller isolation: active=%v last=%q", active, last)
	}
	active, last, _ = reader.PolicyClientRuntime("caller-a", "codex:other-thread", time.Now())
	if len(active) != 0 || last != "" {
		t.Fatal("another thread's selection was exposed")
	}
	m.MarkResult(context.Background(), Result{AuthID: "a", Provider: "codex", Options: optsA, Success: false})
	b, _ := m.GetByID("b")
	publishSelectedAuthMetadata(optsA.Metadata, b)
	m.MarkResult(context.Background(), Result{AuthID: "b", Provider: "codex", Options: optsA, Success: true})
	leaseA.release()
	active, last, completedAt := reader.PolicyClientRuntime("caller-a", "codex:thread-one", time.Now())
	if len(active) != 0 || last != "b" || completedAt.IsZero() {
		t.Fatalf("failed attempt replaced successful account: active=%v last=%q completed=%v", active, last, completedAt)
	}
	m.MarkResult(context.Background(), Result{AuthID: "b", Provider: "codex", Success: true})
	_, last, _ = reader.PolicyClientRuntime("caller-a", "codex:thread-one", time.Now())
	if last != "b" {
		t.Fatal("another request's accounting generation invalidated this session")
	}
	_, last, _ = reader.PolicyClientRuntime("caller-a", "codex:thread-one", time.Now().Add(25*time.Hour))
	if last != "" {
		t.Fatal("expired selection history was reported as current")
	}
}

// An old in-flight credential must not be relabeled as a replacement account.
func TestPolicyClientRuntimeRejectsInFlightIdentityReplacement(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a, _ := m.Register(context.Background(), &Auth{ID: "same-id", Provider: "codex", Metadata: map[string]any{"account_id": "old-account"}})
	headers := make(http.Header)
	headers.Set("Session_id", "thread-one")
	opts, lease := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{Headers: headers, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller"}}, policyRequestDemand{})
	defer lease.release()
	publishSelectedAuthMetadata(opts.Metadata, a)
	if _, err := m.Register(context.Background(), &Auth{ID: "same-id", Provider: "codex", Metadata: map[string]any{"account_id": "replacement-account"}}); err != nil {
		t.Fatal(err)
	}
	active, _, _ := m.PolicyClientRuntime("caller", "codex:thread-one", time.Now())
	if len(active) != 0 {
		t.Fatal("old request was attributed to a replacement identity")
	}
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: "codex", Success: true, Options: opts})
	_, last, _ := m.PolicyClientRuntime("caller", "codex:thread-one", time.Now())
	if last != "" {
		t.Fatal("old successful completion was attributed to replacement account")
	}
}

func TestPolicyClientLateOldCompletionPreservesReplacementSuccess(t *testing.T) {
	m := NewManager(nil, nil, nil)
	headers := make(http.Header)
	headers.Set("Session_id", "thread-one")
	options := func(a *Auth) cliproxyexecutor.Options {
		opts := cliproxyexecutor.Options{Headers: headers, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller"}}
		publishSelectedAuthMetadata(opts.Metadata, a)
		return opts
	}
	a, _ := m.Register(context.Background(), &Auth{ID: "same-id", Provider: "codex", Metadata: map[string]any{"account_id": "old"}})
	oldOpts := options(a)
	b, _ := m.Register(context.Background(), &Auth{ID: "same-id", Provider: "codex", Metadata: map[string]any{"account_id": "new"}})
	newOpts := options(b)
	m.MarkResult(context.Background(), Result{AuthID: b.ID, Provider: "codex", Success: true, Options: newOpts})
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: "codex", Success: true, Options: oldOpts})
	_, last, _ := m.PolicyClientRuntime("caller", "codex:thread-one", time.Now())
	if last != "same-id" {
		t.Fatal("late obsolete completion discarded replacement's valid success")
	}
}
