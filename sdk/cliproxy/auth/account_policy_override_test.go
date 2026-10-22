package auth

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"net/http"
	"testing"
)

type overrideSource struct {
	*policyTestSource
	preferred string
}

func (p *overrideSource) PreferredAccount(provider string) string {
	if provider == "codex" {
		return p.preferred
	}
	return ""
}
func TestTemporaryPreferenceBeatsDeadlineAndReserveButKeepsQuota(t *testing.T) {
	s, p, auths, _ := deadlineFixture(t)
	s.policy = &overrideSource{p, "b"}
	p.settings.Accounts = map[string]accountpolicy.AccountControl{"b": {ReservePercent: 95}}
	a, err := s.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{}, auths)
	if err != nil || a.ID != "b" {
		t.Fatalf("preference %v %v", a, err)
	}
	b := p.snapshots["b"]
	b.Buckets[0].UsedPercent = 100
	p.snapshots["b"] = b
	a, err = s.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{}, auths)
	if err != nil || a.ID != "a" {
		t.Fatalf("hard quota %v %v", a, err)
	}
}

func TestTemporaryPreferenceKeepsEstablishedContinuationAndActiveOwner(t *testing.T) {
	for _, body := range []string{`{"previous_response_id":"resp-a","input":"continue"}`, `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, `{"input":"replayable but active"}`} {
		t.Run(body, func(t *testing.T) {
			s, p, auths, _ := deadlineFixture(t)
			source := &overrideSource{policyTestSource: p}
			s.policy = source
			opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{"thread"}}, OriginalRequest: []byte(`{"input":"hello"}`), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "client"}}
			a, err := s.Pick(context.Background(), "codex", "model", opts, auths)
			if err != nil || a.ID != "a" {
				t.Fatalf("initial owner %v %v", a, err)
			}
			source.preferred = "b"
			s.SetIdleCheck(func(string) bool { return false })
			opts.OriginalRequest = []byte(body)
			a, err = s.Pick(context.Background(), "codex", "model", opts, auths)
			if err != nil || a.ID != "a" {
				t.Fatalf("override migrated active/account-local owner %v %v", a, err)
			}
		})
	}
}
