package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type policyPoolLeaseExecutor struct {
	*openAICompatPoolExecutor
	manager  *Manager
	activeMu sync.Mutex
	active   []int
}

func (e *policyPoolLeaseExecutor) observe(auth *Auth) {
	e.activeMu.Lock()
	defer e.activeMu.Unlock()
	e.active = append(e.active, e.manager.PolicyRuntime(auth.ID).ActiveRequests)
}

func (e *policyPoolLeaseExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.observe(auth)
	return e.openAICompatPoolExecutor.Execute(ctx, auth, req, opts)
}

func (e *policyPoolLeaseExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.observe(auth)
	return e.openAICompatPoolExecutor.CountTokens(ctx, auth, req, opts)
}

func (e *policyPoolLeaseExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.observe(auth)
	return e.openAICompatPoolExecutor.ExecuteStream(ctx, auth, req, opts)
}

func TestPolicyRuntimeReacquiresLeaseForModelPoolRetry(t *testing.T) {
	for _, transport := range []string{"http", "count", "stream"} {
		t.Run(transport, func(t *testing.T) {
			upstreamErr := &Error{HTTPStatus: http.StatusInternalServerError, Message: "first model failed"}
			base := &openAICompatPoolExecutor{id: openAICompatPoolProviderKey,
				executeErrors:     map[string]error{"first": upstreamErr},
				countErrors:       map[string]error{"first": upstreamErr},
				streamFirstErrors: map[string]error{"first": upstreamErr},
			}
			manager := newOpenAICompatPoolTestManager(t, "public", []internalconfig.OpenAICompatibilityModel{{Name: "first", Alias: "public"}, {Name: "second", Alias: "public"}}, base)
			executor := &policyPoolLeaseExecutor{openAICompatPoolExecutor: base, manager: manager}
			manager.RegisterExecutor(executor)
			request := cliproxyexecutor.Request{Model: "public"}
			var err error
			switch transport {
			case "http":
				_, err = manager.Execute(context.Background(), []string{openAICompatPoolProviderKey}, request, cliproxyexecutor.Options{})
			case "count":
				_, err = manager.ExecuteCount(context.Background(), []string{openAICompatPoolProviderKey}, request, cliproxyexecutor.Options{})
			case "stream":
				var stream *cliproxyexecutor.StreamResult
				stream, err = manager.ExecuteStream(context.Background(), []string{openAICompatPoolProviderKey}, request, cliproxyexecutor.Options{})
				if err == nil {
					readOpenAICompatStreamPayload(t, stream)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			executor.activeMu.Lock()
			defer executor.activeMu.Unlock()
			if len(executor.active) != 2 || executor.active[0] != 1 || executor.active[1] != 1 {
				t.Fatalf("active inference leases = %v, want [1 1]", executor.active)
			}
		})
	}
}

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
