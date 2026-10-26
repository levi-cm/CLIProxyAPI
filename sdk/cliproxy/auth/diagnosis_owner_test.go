package auth

import (
	"context"
	"testing"
	"time"
)

func TestDiagnosisOwnerOnQuotaCooldown(t *testing.T) {
	s, _, auths, _ := durableBindingFixture(t)
	successfulBinding(t, s, auths, bindingOptions("diagnosis-thread", "caller-one", `{"input":"hello"}`))
	auths[0].ModelStates = map[string]*ModelState{"gpt-5": {Unavailable: true, Status: StatusError, NextRetryAfter: time.Now().Add(time.Hour), Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: time.Now().Add(time.Hour)}}}
	opts := bindingOptions("diagnosis-thread", "caller-one", `{"input":[{"type":"reasoning","encrypted_content":"opaque"},{"role":"user","content":"continue"}]}`)
	_, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	t.Logf("saved owner in cooldown: %v", err)
	e, ok := err.(*Error)
	if !ok || e.Code != "unsafe_affinity_unavailable" {
		t.Fatalf("want unavailable saved owner, not unknown owner: %v", err)
	}
}

func TestDiagnosisOwnerRetainedWhenDeadlinePreferenceChanges(t *testing.T) {
	s, p, auths, now := durableBindingFixture(t)
	successfulBinding(t, s, auths, bindingOptions("diagnosis-thread", "caller-one", `{"input":"hello"}`))
	addExpiringCredit(p, now)
	opts := bindingOptions("diagnosis-thread", "caller-one", `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`)
	got, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || got == nil || got.ID != "a" {
		t.Fatalf("deadline preference lost original owner: %v %v", got, err)
	}
	t.Log("encrypted continuation retained original account despite preference for the other account")
}

func TestDiagnosisTemporaryCapacityMisclassifiedAsQuota(t *testing.T) {
	m := NewManager(nil, nil, nil)
	_, err := m.Register(context.Background(), &Auth{ID: "diagnosis-a", Provider: "codex", Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	m.MarkResult(context.Background(), Result{AuthID: "diagnosis-a", Provider: "codex", Model: "diagnosis-model", Success: false, Error: &Error{HTTPStatus: 429, Message: `{"error":{"code":"turn_rate_limit_exceeded","type":"rate_limit_error","message":"Codex is at capacity, please try again later"}}`}})
	a, ok := m.GetByID("diagnosis-a")
	if !ok {
		t.Fatal("registered auth missing")
	}
	state := a.ModelStates["diagnosis-model"]
	if state != nil && (state.Quota.Exceeded || state.Quota.Reason == "quota" || state.Unavailable) {
		t.Fatalf("capacity changed durable quota or availability: %+v", state)
	}
	if a.Quota.Exceeded || a.Disabled {
		t.Fatal("capacity exhausted or disabled credential")
	}

}

func TestDiagnosisToolSchemaIsNotContinuationState(t *testing.T) {
	for _, property := range []string{"conversation_id", "previous_response_id", "encrypted_content", "turn_state"} {
		t.Run(property, func(t *testing.T) {
			s, _, auths, _ := durableBindingFixture(t)
			body := `{"input":"Hello, this is a brand new chat.","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"` + property + `":{"type":"string"}}}}]}`
			opts := bindingOptions("new-chat-with-tools", "caller-one", body)
			got, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths)
			t.Logf("new chat with schema property %q: owner=%v error=%v local_state=%v", property, got, err, policyRequestHasLocalState(opts))
			if err != nil {
				t.Errorf("new chat rejected due to a tool schema, without any actual continuation: %v", err)
			}
		})
	}
}
