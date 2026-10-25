package auth

import (
	"context"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"net/http"
	"testing"
)

type scopedGuardianFallback struct {
	FillFirstSelector
	parentCalls int
}

func (s *scopedGuardianFallback) AffinityNamespace(provider string, opts cliproxyexecutor.Options) string {
	return provider + "::" + opts.Metadata["caller"].(string)
}
func (s *scopedGuardianFallback) PickParent(_ context.Context, _, _ string, _ cliproxyexecutor.Options, _ string, bound *Auth, _ []*Auth) (*Auth, error) {
	s.parentCalls++
	return bound, nil
}

func TestPolicyGuardianHonorsCallerModelAndParentValidation(t *testing.T) {
	for _, tc := range []struct {
		name, caller, model, want string
		validations               int
	}{
		{"same scope", "alice", "parent-model", "b", 1},
		{"other caller", "bob", "parent-model", "a", 0},
		{"other model", "alice", "other-model", "a", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fallback := &scopedGuardianFallback{}
			selector := NewSessionAffinitySelector(fallback)
			defer selector.Stop()
			a, b := &Auth{ID: "a", Provider: "codex"}, &Auth{ID: "b", Provider: "codex"}
			parent := cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{"parent"}}, Metadata: map[string]any{"caller": "alice"}}
			if _, err := selector.Pick(context.Background(), "codex", "parent-model", parent, []*Auth{b}); err != nil {
				t.Fatal(err)
			}
			child := guardianTestOptions("child", "parent")
			child.Metadata["caller"] = tc.caller
			picked, err := selector.Pick(context.Background(), "codex", tc.model, child, []*Auth{a, b})
			if err != nil {
				t.Fatal(err)
			}
			if picked == nil || picked.ID != tc.want || fallback.parentCalls != tc.validations {
				t.Fatalf("picked=%v parent validations=%d, want %s/%d", picked, fallback.parentCalls, tc.want, tc.validations)
			}
		})
	}
}
