package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestPolicyRecoveryFencesActivePreResetQuotaResults(t *testing.T) {
	for _, hadCooldown := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy-active", true: "blocked-active"}[hadCooldown], func(t *testing.T) {
			m, before, after := recoveryFixture()
			if !hadCooldown {
				resetModelState(m.auths["a"].ModelStates["gpt-5"], before.ObservedAt)
				m.auths["a"].LastError = nil
			}
			old := m.nextExecutionSeq()
			if err := m.RecoverPolicyQuota(context.Background(), before, after); err != nil {
				t.Fatal(err)
			}
			ctx := logging.WithResponseHeadersHolder(context.Background())
			logging.SetResponseHeaders(ctx, http.Header{"X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Used-Percent": {"100"}})
			m.MarkResult(ctx, Result{AuthID: "a", Provider: "codex", Model: "gpt-5", Error: &Error{HTTPStatus: 429}, ExecutionSeq: old})
			got, _ := m.GetByID("a")
			if got.ModelStates["gpt-5"].Quota.Exceeded || policyQuotaExhausted(got.Quota) || got.Failed != 1 {
				t.Fatal("pre-reset result reinstated old quota or lost accounting")
			}
			m.MarkResult(ctx, Result{AuthID: "a", Provider: "codex", Model: "gpt-5", Success: true, ExecutionSeq: old})
			got, _ = m.GetByID("a")
			if policyQuotaExhausted(got.Quota) || got.Success != 1 {
				t.Fatal("pre-reset success reinstated old quota headers or lost accounting")
			}
			fresh := m.nextExecutionSeq()
			m.MarkResult(context.Background(), Result{AuthID: "a", Provider: "codex", Model: "gpt-5", Error: &Error{HTTPStatus: 429}, ExecutionSeq: fresh})
			got, _ = m.GetByID("a")
			if !got.ModelStates["gpt-5"].Quota.Exceeded {
				t.Fatal("genuine post-reset quota ignored")
			}
		})
	}
}

func TestPolicyRecoveryFenceRetainsUncoveredAndAuthenticationFailures(t *testing.T) {
	for _, model := range []string{"gpt-5", "unrelated"} {
		m, before, after := recoveryFixture()
		old := m.nextExecutionSeq()
		if err := m.RecoverPolicyQuota(context.Background(), before, after); err != nil {
			t.Fatal(err)
		}
		status := 429
		if model == "gpt-5" {
			status = 401
		}
		m.MarkResult(context.Background(), Result{AuthID: "a", Provider: "codex", Model: model, Error: &Error{HTTPStatus: status}, ExecutionSeq: old})
		got, _ := m.GetByID("a")
		if statusCodeFromResult(got.ModelStates[model].LastError) != status {
			t.Fatal("uncovered/authentication failure hidden by quota fence")
		}
	}
}

func TestFailedPolicyRecoveryDoesNotPublishQuotaFence(t *testing.T) {
	m, before, after := recoveryFixture()
	m.nextExecutionSeq()
	m.SetCooldownStateStore(&policyRecoveryFailingStore{fail: true})
	if err := m.RecoverPolicyQuota(context.Background(), before, after); err == nil {
		t.Fatal("expected persistence failure")
	}
	if len(m.policyQuotaRecovery) != 0 {
		t.Fatal("failed recovery published quota fence")
	}
}

func TestGlobalPolicyRecoveryFenceCoversPreviouslyUnknownModel(t *testing.T) {
	m, before, after := recoveryFixture()
	before.Buckets[0].Model = ""
	after.Buckets[0].Model = ""
	old := m.nextExecutionSeq()
	if err := m.RecoverPolicyQuota(context.Background(), before, after); err != nil {
		t.Fatal(err)
	}
	m.MarkResult(context.Background(), Result{AuthID: "a", Provider: "codex", Model: "new-model", Error: &Error{HTTPStatus: 429}, CredentialScope: true, ExecutionSeq: old})
	got, _ := m.GetByID("a")
	if got.Quota.Exceeded || (got.ModelStates["new-model"] != nil && got.ModelStates["new-model"].Quota.Exceeded) {
		t.Fatal("pre-reset credential quota reinstated")
	}
	// Runtime fences never impose a wait or change the successful reset's generation.
	if got.NextRetryAfter.After(time.Now()) {
		t.Fatal("old failure introduced a new cooldown")
	}
}
