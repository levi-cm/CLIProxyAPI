package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

func TestPolicyContinuationProtocolFields(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"previous_response_id":"resp-secret"}`, true},
		{`{"conversation":{"id":"conv-secret"}}`, true},
		{`{"input":[{"type":"reasoning","encrypted_content":"cipher-secret"}]}`, true},
		{`{"input":[{"type":"compaction","encrypted_content":"cipher-secret"}]}`, true},
		{`{"input":[{"type":"item_reference","id":"item-secret"}]}`, true},
		{`{"type":"response.create","response":{"previous_response_id":"resp-secret"}}`, true},
		{`{"input":[{"type":"function_call_output","output":{"encrypted_content":"ordinary-data","conversation_id":"ordinary-data"}}]}`, false},
		{`{"input":[{"role":"user","content":[{"type":"input_text","text":"{\"previous_response_id\":\"ordinary-text\"}"}]}]}`, false},
		{`{"tool_choice":{"type":"function","function":{"name":"conversation_id"}},"metadata":{"turn_state":"ordinary-data"},"input":"hello"}`, false},
	} {
		opts := bindingOptions("private-session", "caller-one", tc.body)
		if got := policyRequestHasLocalState(opts); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.body, got, tc.want)
		}
	}
}

func TestPolicyRejectionDiagnosticsSanitized(t *testing.T) {
	hook := test.NewGlobal()
	defer hook.Reset()
	s, _, auths, _ := durableBindingFixture(t)
	ctx := logging.WithRequestID(context.Background(), "fixture-trace")
	_, err := s.Pick(ctx, "codex", "gpt-5", bindingOptions("private-session", "caller-one", `{"input":[{"type":"reasoning","encrypted_content":"cipher-secret"}]}`), auths)
	if err == nil {
		t.Fatal("unowned encrypted state accepted")
	}
	var entry *log.Entry
	for _, e := range hook.AllEntries() {
		if e.Data["component"] == "account-policy-rejection" {
			entry = e
		}
	}
	if entry == nil {
		t.Fatal("missing sanitized rejection diagnostic")
	}
	if entry.Data["continuation"] != "reasoning.encrypted_content" || entry.Data["trace_id"] != "fixture-trace" || entry.Data["binding_lookup"] != "miss" {
		t.Fatalf("diagnostic fields: %v", entry.Data)
	}
	out, _ := entry.String()
	for _, secret := range []string{"private-session", "caller-one", "cipher-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("diagnostic leaked %s", secret)
		}
	}
}

func TestPolicyReasoningEnvelopeRemainsAccountLocal(t *testing.T) {
	opts := bindingOptions("private-session", "caller-one", `{"input":"hello","reasoning":{"encrypted_content":"cipher-secret"}}`)
	if !policyRequestHasLocalState(opts) {
		t.Fatal("supported reasoning envelope lost ownership protection")
	}
}
