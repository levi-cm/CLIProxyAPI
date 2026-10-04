package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type acrossPriorityTestSelector struct{}

func (acrossPriorityTestSelector) SelectorWantsAcrossPriorities() bool { return true }
func (acrossPriorityTestSelector) Pick(_ context.Context, _, _ string, _ cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	return auths[len(auths)-1], nil
}

// A dynamic selector must see eligible lower priorities before ranking deadlines.
func TestManagerDynamicSelectorReceivesAllPriorities(t *testing.T) {
	m := NewManager(nil, acrossPriorityTestSelector{}, nil)
	high := &Auth{ID: "a", Provider: "codex", Attributes: map[string]string{"priority": "10"}}
	low := &Auth{ID: "b", Provider: "codex", Attributes: map[string]string{"priority": "0"}}
	_, candidates, err := m.availableAuthsForSelector(m.Selector(), []*Auth{high, low}, "codex", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("dynamic selector received %d candidates, want both priorities", len(candidates))
	}
}

func TestAffinityDynamicFallbackReceivesAllPriorities(t *testing.T) {
	s := NewSessionAffinitySelector(acrossPriorityTestSelector{})
	defer s.Stop()
	high := &Auth{ID: "a", Provider: "codex", Attributes: map[string]string{"priority": "10"}}
	low := &Auth{ID: "b", Provider: "codex", Attributes: map[string]string{"priority": "0"}}
	got, err := s.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{high, low})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("picked %q, want dynamic lower-priority b", got.ID)
	}
}
