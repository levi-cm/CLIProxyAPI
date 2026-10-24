package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type splitSource struct {
	*policyTestSource
	id     string
	shares []accountpolicy.RoutingShare
}

func (p *splitSource) RoutingSplit(provider string) (string, []accountpolicy.RoutingShare) {
	if provider != "codex" {
		return "", nil
	}
	return p.id, p.shares
}

func TestLegacySplitSharesDoNotCreatePercentageTargets(t *testing.T) {
	for _, percent := range []int{1, 37, 50, 99} {
		t.Run(fmt.Sprint(percent), func(t *testing.T) {
			s, p, auths, _ := deadlineFixture(t)
			s.policy = &splitSource{p, "legacy", []accountpolicy.RoutingShare{{CredentialID: "a", Percent: percent}, {CredentialID: "b", Percent: 100 - percent}}}
			m := NewManager(nil, s, nil)
			var leases []*policyRequestLease
			defer func() {
				for _, l := range leases {
					l.release()
				}
			}()
			for i, want := range []string{"a", "a", "b", "a", "b"} {
				opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
				leases = append(leases, l)
				got, err := s.Pick(context.Background(), "codex", fmt.Sprintf("model-%d", i), opts, auths)
				if err != nil || got.ID != want {
					t.Fatalf("legacy %d at %d: %v %v want %s", percent, i, got, err, want)
				}
			}
			if auths[0].Attributes[AttributeWeight] != "" || auths[1].Attributes[AttributeWeight] != "" {
				t.Fatal("mutated credential weights")
			}
		})
	}
}

func TestLegacySplitBurstUsesAtomicPressureAcrossModels(t *testing.T) {
	s, p, auths, _ := deadlineFixture(t)
	source := &splitSource{p, "first", []accountpolicy.RoutingShare{{CredentialID: "a", Percent: 1}, {CredentialID: "b", Percent: 99}}}
	s.policy = source
	m := NewManager(nil, s, nil)
	leases := make([]*policyRequestLease, 101)
	var wg sync.WaitGroup
	for i := range leases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
			leases[i] = l
			if _, err := s.Pick(context.Background(), "codex", fmt.Sprintf("model-%d", i%3), opts, auths); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	for _, l := range leases {
		defer l.release()
	}
	if a, b := m.PolicyRuntime("a").ActiveRequests, m.PolicyRuntime("b").ActiveRequests; a != 51 || b != 50 {
		t.Fatalf("percentage target survived: %d/%d", a, b)
	}
	source.id = "replacement"
	source.shares = []accountpolicy.RoutingShare{{CredentialID: "a", Percent: 99}, {CredentialID: "b", Percent: 1}}
	for _, l := range leases {
		l.release()
	}
	opts, l := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
	defer l.release()
	got, err := s.Pick(context.Background(), "codex", "model", opts, auths)
	if err != nil || got.ID != "a" {
		t.Fatalf("replacement created catch-up debt: %v %v", got, err)
	}
}

func TestSplitSafeBoundariesAndProtectedOwners(t *testing.T) {
	for _, kind := range []string{"safe", "encrypted", "active", "strict"} {
		t.Run(kind, func(t *testing.T) {
			s, p, auths, _ := deadlineFixture(t)
			source := &splitSource{policyTestSource: p}
			s.policy = source
			opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": {"split-thread"}}, OriginalRequest: []byte(`{"input":"self-contained"}`), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "client"}}
			owner, err := s.Pick(context.Background(), "codex", "model", opts, auths)
			if err != nil || owner.ID != "a" {
				t.Fatalf("initial owner: %v", err)
			}
			source.id, source.shares = "split", []accountpolicy.RoutingShare{{CredentialID: "a", Percent: 50}, {CredentialID: "b", Percent: 50}}
			switch kind {
			case "encrypted":
				opts.OriginalRequest = []byte(`{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`)
			case "active":
				s.SetIdleCheck(func(string) bool { return false })
			case "strict":
				p.settings.Affinity = "strict"
			}
			counts := map[string]int{}
			for range 100 {
				a, err := s.Pick(context.Background(), "codex", "model", opts, auths)
				if err != nil || a == nil {
					t.Fatalf("continuation: %v", err)
				}
				counts[a.ID]++
			}
			if counts["a"] != 100 {
				t.Fatalf("spreading migrated bound owner: %+v", counts)
			}
		})
	}
}

func TestSplitUnavailableMemberDoesNotAccumulateCatchup(t *testing.T) {
	s, p, auths, _ := deadlineFixture(t)
	s.policy = &splitSource{p, "split", []accountpolicy.RoutingShare{{CredentialID: "a", Percent: 37}, {CredentialID: "b", Percent: 63}}}
	for range 20 {
		a, err := s.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{}, auths[:1])
		if err != nil || a.ID != "a" {
			t.Fatalf("available member: %v", err)
		}
	}
	counts := map[string]int{}
	for range 100 {
		a, err := s.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatal(err)
		}
		counts[a.ID]++
	}
	if counts["a"] != 100 || counts["b"] != 0 {
		t.Fatalf("catch-up skew: %+v", counts)
	}
}
