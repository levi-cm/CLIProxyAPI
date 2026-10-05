package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicybindings"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func durableBindingFixture(t *testing.T) (*EarliestDeadlineSelector, *policyTestSource, []*Auth, time.Time) {
	t.Helper()
	s, p, auths, now := deadlineFixture(t)
	p.settings.StateDir = t.TempDir()
	for _, a := range auths {
		a.Metadata = map[string]any{"account_id": "account-" + a.ID, "workspace_id": "account-" + a.ID}
		a.RegistrationEpoch = 4
		v := p.snapshots[a.ID]
		v.Identity.WorkspaceID = "account-" + a.ID
		p.snapshots[a.ID] = v
	}
	setBindingResolver(s, auths)
	return s, p, auths, now
}

func setBindingResolver(s *EarliestDeadlineSelector, auths []*Auth) {
	s.SetBindingAuthResolver(func(id string) (*Auth, bool) {
		for _, a := range auths {
			if a.ID == id {
				return a.Clone(), true
			}
		}
		return nil, false
	})
}

func bindingOptions(session, caller, body string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{session}}, OriginalRequest: []byte(body), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: caller}}
}

func successfulBinding(t *testing.T, s *EarliestDeadlineSelector, auths []*Auth, opts cliproxyexecutor.Options) string {
	t.Helper()
	a, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil {
		t.Fatal(err)
	}
	s.OnResult(Result{AuthID: a.ID, Provider: "codex", Model: "gpt-5", Success: true, Options: opts})
	return a.ID
}

// A completed request must retain its exact owner after both cache expiry and restart.
func TestPolicyBindingRestartResumesEncryptedCodexConversation(t *testing.T) {
	s, p, auths, now := durableBindingFixture(t)
	successfulBinding(t, s, auths, bindingOptions("resume-thread", "caller-one", `{"input":"hello"}`))
	addExpiringCredit(p, now) // Ranking now prefers B, but encrypted state still belongs to A.
	for _, a := range auths {
		a.RegistrationEpoch++ // Epoch is process-local, not durable identity.
	}
	restarted := NewEarliestDeadlineSelector(p, &FillFirstSelector{}, func() time.Time { return now.Add(2 * time.Hour) })
	t.Cleanup(restarted.Stop)
	opts := bindingOptions("resume-thread", "caller-one", `{"input":[{"type":"reasoning","encrypted_content":"opaque"},{"role":"user","content":"continue"}]}`)
	a, err := restarted.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || a == nil || a.ID != "a" {
		t.Fatalf("resumed owner=%v error=%v; want original A, never new deadline B", a, err)
	}
}

func TestPolicyBindingRestartIsolation(t *testing.T) {
	for _, change := range []string{"caller", "session", "model", "credential", "account", "workspace", "claims", "unavailable"} {
		t.Run(change, func(t *testing.T) {
			s, p, auths, now := durableBindingFixture(t)
			successfulBinding(t, s, auths, bindingOptions("resume-thread", "caller-one", `{"input":"hello"}`))
			opts := bindingOptions("resume-thread", "caller-one", `{"previous_response_id":"resp-owned","input":"continue"}`)
			model := "gpt-5"
			switch change {
			case "caller":
				opts.Metadata[cliproxyexecutor.CallerScopeMetadataKey] = "caller-two"
			case "session":
				opts.Headers.Set("Session_id", "other-thread")
			case "model":
				model = "other-model"
			case "credential":
				auths[0].ID = "replacement-file"
			case "account":
				auths[0].Metadata["account_id"] = "other-account"
			case "workspace":
				auths[0].Metadata["workspace_id"] = "other-workspace"
			case "claims":
				auths[0].Metadata["access_token"] = "invalid.jwt.claims"
			case "unavailable":
				p.settings.Accounts["a"] = accountpolicy.AccountControl{Hold: true}
			}
			restarted := NewEarliestDeadlineSelector(p, &FillFirstSelector{}, func() time.Time { return now })
			t.Cleanup(restarted.Stop)
			if a, err := restarted.Pick(context.Background(), "codex", model, opts, auths); err == nil || a != nil {
				t.Fatalf("%s crossed ownership boundary: owner=%v error=%v", change, a, err)
			}
		})
	}
}

func TestPolicyBindingRestartForkInheritsValidatedParent(t *testing.T) {
	s, p, auths, now := durableBindingFixture(t)
	successfulBinding(t, s, auths, bindingOptions("parent-thread", "caller-one", `{"input":"hello"}`))
	addExpiringCredit(p, now)
	child := bindingOptions("child-thread", "caller-one", `{"metadata":{"parent_thread_id":"parent-thread"},"input":[{"type":"reasoning","encrypted_content":"parent-state"}]}`)
	child.Headers.Set("X-Openai-Subagent", "true")
	restarted := NewEarliestDeadlineSelector(p, &FillFirstSelector{}, func() time.Time { return now })
	t.Cleanup(restarted.Stop)
	setBindingResolver(restarted, auths)
	if owner := successfulBinding(t, restarted, auths, child); owner != "a" {
		t.Fatalf("fork inherited %q, want A", owner)
	}
	// A child persists its own owner and can resume without its parent header.
	again := NewEarliestDeadlineSelector(p, &FillFirstSelector{}, func() time.Time { return now })
	t.Cleanup(again.Stop)
	resume := bindingOptions("child-thread", "caller-one", `{"input":[{"type":"reasoning","encrypted_content":"child-state"}]}`)
	a, err := again.Pick(context.Background(), "codex", "gpt-5", resume, auths)
	if err != nil || a == nil || a.ID != "a" {
		t.Fatalf("child resume=%v error=%v", a, err)
	}
}

func TestPolicyBindingFailedRequestDoesNotEstablishDurableOwner(t *testing.T) {
	s, p, auths, now := durableBindingFixture(t)
	opts := bindingOptions("failed-thread", "caller-one", `{"input":"hello"}`)
	a, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil {
		t.Fatal(err)
	}
	s.OnResult(Result{AuthID: a.ID, Provider: "codex", Model: "gpt-5", Success: false, Options: opts})
	restarted := NewEarliestDeadlineSelector(p, &FillFirstSelector{}, func() time.Time { return now })
	t.Cleanup(restarted.Stop)
	if a, err = restarted.Pick(context.Background(), "codex", "gpt-5", bindingOptions("failed-thread", "caller-one", `{"previous_response_id":"unknown","input":"continue"}`), auths); err == nil || a != nil {
		t.Fatalf("failed request invented owner=%v error=%v", a, err)
	}
}

func TestPolicyBindingDurableConflictNeverUsesStaleCache(t *testing.T) {
	s, p, auths, _ := durableBindingFixture(t)
	opts := bindingOptions("same-thread", "caller-one", `{"input":"hello"}`)
	successfulBinding(t, s, auths, opts)
	key := accountpolicybindings.NewKey(policyClientScope(opts), "codex", "codex:same-thread", "gpt-5")
	a, _ := durableOwner(auths[0])
	b, _ := durableOwner(auths[1])
	if err := accountpolicybindings.New(p.settings.StateDir).Put(key, b, &a); err != nil {
		t.Fatal(err)
	}
	// Another completed writer moved the durable owner; stale local A is unsafe.
	got, err := s.Pick(context.Background(), "codex", "gpt-5", bindingOptions("same-thread", "caller-one", `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`), auths)
	if err == nil && got != nil && got.ID == "a" {
		t.Fatal("encrypted continuation used stale cached owner A despite durable B")
	}
}

func TestPolicyBindingKnownChildDoesNotDependOnObsoleteParent(t *testing.T) {
	s, p, auths, now := durableBindingFixture(t)
	successfulBinding(t, s, auths, bindingOptions("parent-thread", "caller-one", `{"input":"hello"}`))
	child := bindingOptions("child-thread", "caller-one", `{"metadata":{"forked_from_thread_id":"parent-thread"},"input":[{"type":"reasoning","encrypted_content":"parent-state"}]}`)
	successfulBinding(t, s, auths, child)
	parentKey := accountpolicybindings.NewKey(policyClientScope(child), "codex", "codex:parent-thread", "gpt-5")
	a, _ := durableOwner(auths[0])
	b, _ := durableOwner(auths[1])
	if err := accountpolicybindings.New(p.settings.StateDir).Put(parentKey, b, &a); err != nil {
		t.Fatal(err)
	}
	p.settings.Accounts["b"] = accountpolicy.AccountControl{Hold: true}
	restarted := NewEarliestDeadlineSelector(p, &FillFirstSelector{}, func() time.Time { return now })
	t.Cleanup(restarted.Stop)
	got, err := restarted.Pick(context.Background(), "codex", "gpt-5", child, auths)
	if err != nil || got == nil || got.ID != "a" {
		t.Fatalf("known child's own A rejected due to obsolete parent B: %v %v", got, err)
	}
}

func TestPolicyBindingReadOnlyResetModeStillRecordsObservationOwnership(t *testing.T) {
	s, p, auths, now := durableBindingFixture(t)
	p.settings.ReadOnly = true // Provider resets are read-only; local observations still persist.
	successfulBinding(t, s, auths, bindingOptions("read-only", "caller-one", `{"input":"hello"}`))
	restarted := NewEarliestDeadlineSelector(p, &FillFirstSelector{}, func() time.Time { return now })
	t.Cleanup(restarted.Stop)
	got, err := restarted.Pick(context.Background(), "codex", "gpt-5", bindingOptions("read-only", "caller-one", `{"previous_response_id":"resp-a","input":"continue"}`), auths)
	if err != nil || got == nil || got.ID != "a" {
		t.Fatalf("observational ownership lost in reset-read-only mode: %v %v", got, err)
	}
}

func TestPolicyBindingReplayableUnavailableOwnerCanMigrate(t *testing.T) {
	s, p, auths, _ := durableBindingFixture(t)
	successfulBinding(t, s, auths, bindingOptions("replayable", "caller-one", `{"input":"hello"}`))
	p.settings.Accounts["a"] = accountpolicy.AccountControl{Hold: true}
	got, err := s.Pick(context.Background(), "codex", "gpt-5", bindingOptions("replayable", "caller-one", `{"input":[{"role":"user","content":"complete self-contained input"}]}`), auths)
	if err != nil || got == nil || got.ID != "b" {
		t.Fatalf("safe-boundary fallback lost: %v %v", got, err)
	}
}

func TestPolicyBindingObsoleteCompletionCannotPersistReplacementIdentity(t *testing.T) {
	s, p, auths, now := durableBindingFixture(t)
	opts := bindingOptions("replaced", "caller-one", `{"input":"hello"}`)
	a, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil {
		t.Fatal(err)
	}
	// Re-registration occurred while upstream was processing the old account.
	auths[0].RegistrationEpoch++
	auths[0].Metadata["account_id"] = "new-account"
	auths[0].Metadata["workspace_id"] = "new-account"
	s.OnResult(Result{AuthID: a.ID, Provider: "codex", Model: "gpt-5", Success: true, Options: opts})
	restarted := NewEarliestDeadlineSelector(p, &FillFirstSelector{}, func() time.Time { return now })
	t.Cleanup(restarted.Stop)
	got, err := restarted.Pick(context.Background(), "codex", "gpt-5", bindingOptions("replaced", "caller-one", `{"previous_response_id":"old-account-response","input":"continue"}`), auths)
	if err == nil || got != nil {
		t.Fatalf("obsolete success established a replaced owner: %v %v", got, err)
	}
}
