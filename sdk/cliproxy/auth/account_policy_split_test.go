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

func TestSplitAllPercentagesIgnoreDeadlineAndStaticWeights(t *testing.T) {
	for percent := 1; percent <= 99; percent++ {
		s, p, auths, _ := deadlineFixture(t)
		s.policy = &splitSource{p, "split", []accountpolicy.RoutingShare{{"a", percent}, {"b", 100 - percent}}}
		p.settings.Accounts = map[string]accountpolicy.AccountControl{"b": {ReservePercent: 95}}
		counts := map[string]int{}
		for range 100 {
			a, err := s.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{}, auths)
			if err != nil || a == nil {
				t.Fatalf("split %d: %v", percent, err)
			}
			counts[a.ID]++
		}
		if counts["a"] != percent || counts["b"] != 100-percent {
			t.Fatalf("split %d/100: %+v", percent, counts)
		}
		if auths[0].Attributes[AttributeWeight] != "" || auths[1].Attributes[AttributeWeight] != "" {
			t.Fatal("split mutated credential weights")
		}
	}
}

func TestSplitConcurrentDecisionsAndReplacement(t *testing.T) {
	s, p, auths, _ := deadlineFixture(t)
	source := &splitSource{p, "first", []accountpolicy.RoutingShare{{"a", 37}, {"b", 63}}}
	s.policy = source
	counts := map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				a, err := s.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{}, auths)
				if err != nil || a == nil {
					t.Errorf("concurrent pick: %v", err)
					return
				}
				mu.Lock()
				counts[a.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if counts["a"] != 370 || counts["b"] != 630 {
		t.Fatalf("concurrent split: %+v", counts)
	}
	source.id = "replacement"
	source.shares = []accountpolicy.RoutingShare{{"a", 99}, {"b", 1}}
	counts = map[string]int{}
	for range 100 {
		a, err := s.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatal(err)
		}
		counts[a.ID]++
	}
	if counts["a"] != 99 || counts["b"] != 1 {
		t.Fatalf("replacement split: %+v", counts)
	}
}

func TestSplitPercentageIsSharedAcrossModels(t *testing.T) {
	s, p, auths, _ := deadlineFixture(t)
	s.policy = &splitSource{p, "global", []accountpolicy.RoutingShare{{"a", 37}, {"b", 63}}}
	counts := map[string]int{}
	for i := range 100 {
		selected, err := s.Pick(context.Background(), "codex", fmt.Sprintf("model-%d", i), cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatal(err)
		}
		counts[selected.ID]++
	}
	if counts["a"] != 37 || counts["b"] != 63 {
		t.Fatalf("per-model skew: %+v", counts)
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
			source.id, source.shares = "split", []accountpolicy.RoutingShare{{"a", 50}, {"b", 50}}
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
			if kind == "safe" {
				if counts["a"] != 50 || counts["b"] != 50 {
					t.Fatalf("safe continuation split: %+v", counts)
				}
			} else if counts["a"] != 100 {
				t.Fatalf("migrated protected owner: %+v", counts)
			}
		})
	}
}

func TestSplitUnavailableMemberDoesNotAccumulateCatchup(t *testing.T) {
	s, p, auths, _ := deadlineFixture(t)
	s.policy = &splitSource{p, "split", []accountpolicy.RoutingShare{{"a", 37}, {"b", 63}}}
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
	if counts["a"] != 37 || counts["b"] != 63 {
		t.Fatalf("catch-up skew: %+v", counts)
	}
}
