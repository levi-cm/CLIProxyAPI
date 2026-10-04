package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Selection must keep the account busy until the real response boundary.
func TestPolicyRuntimeLeaseProtectsAccountUntilRelease(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.auths["a"] = &Auth{ID: "a", Provider: "codex"}
	m.auths["b"] = &Auth{ID: "b", Provider: "codex"}
	opts, lease := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
	if !m.PolicyIsIdle("a") {
		t.Fatal("account unexpectedly busy before selection")
	}
	publishSelectedAuthMetadata(opts.Metadata, &Auth{ID: "a", Provider: "codex"})
	if m.PolicyIsIdle("a") || !m.PolicyHasDemand("a") {
		t.Fatal("selected account must remain busy")
	}
	if got := m.PolicyRuntime("a").ActiveRequests; got != 1 {
		t.Fatalf("active=%d, want 1", got)
	}
	publishSelectedAuthMetadata(opts.Metadata, &Auth{ID: "b", Provider: "codex"})
	if !m.PolicyIsIdle("a") || m.PolicyIsIdle("b") {
		t.Fatal("retry must transfer accounting after previous attempt ended")
	}
	lease.release()
	lease.release()
	if !m.PolicyIsIdle("b") {
		t.Fatal("completed account remains busy")
	}
}

type policyProxyCapture struct{ proxy string }

func (p *policyProxyCapture) RoundTripperFor(a *Auth) http.RoundTripper {
	p.proxy = a.ProxyURL
	return http.DefaultTransport
}

func TestPolicyCredentialTransportPreservesGlobalProxy(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.auths["a"] = &Auth{ID: "a", Provider: "codex"}
	cfg := &internalconfig.Config{}
	cfg.ProxyURL = "http://127.0.0.1:12345"
	m.SetConfig(cfg)
	provider := &policyProxyCapture{}
	m.SetRoundTripperProvider(provider)
	if m.CredentialRoundTripper("a") == nil || provider.proxy != "http://127.0.0.1:12345" {
		t.Fatalf("discovery bypassed global proxy %q", provider.proxy)
	}
	if m.auths["a"].ProxyURL != "" {
		t.Fatal("transport lookup mutated credential")
	}
}

func TestPolicyResetReservationRejectsBusyAndBlocksNewSelection(t *testing.T) {
	m := NewManager(nil, nil, nil)
	opts, lease := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
	publishSelectedAuthMetadata(opts.Metadata, &Auth{ID: "a"})
	if _, err := m.AcquirePolicyReset(context.Background(), "a"); err == nil {
		t.Fatal("reset admitted active inference")
	}
	lease.release()
	release, err := m.AcquirePolicyReset(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waitingOpts, waitingLease := m.trackPolicyRequest(ctx, cliproxyexecutor.Options{})
	publishSelectedAuthMetadata(waitingOpts.Metadata, &Auth{ID: "a"})
	if m.PolicyRuntime("a").ActiveRequests != 0 {
		t.Fatal("cancelled selection entered reserved account")
	}
	waitingLease.release()
	release()
	release()
	if _, err = m.AcquirePolicyReset(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err = m.AcquirePolicyReset(ctx, "b"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire=%v", err)
	}
}

func TestPendingPolicyDemandCanWaitWhileAccountIdle(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.auths["a"] = &Auth{ID: "a", Provider: "codex"}
	opts, lease := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{}, policyRequestDemand{providers: []string{"codex"}})
	if !m.PolicyHasDemand("a") || !m.PolicyIsIdle("a") {
		t.Fatal("waiting demand must be visible while account has no active inference")
	}
	publishSelectedAuthMetadata(opts.Metadata, &Auth{ID: "a", Provider: "codex"})
	lease.finishAttempt()
	if !m.PolicyHasDemand("a") || !m.PolicyIsIdle("a") {
		t.Fatal("failed attempt awaiting retry lost pending demand")
	}
	lease.release()
	if m.PolicyHasDemand("a") {
		t.Fatal("completed request left demand")
	}
}
