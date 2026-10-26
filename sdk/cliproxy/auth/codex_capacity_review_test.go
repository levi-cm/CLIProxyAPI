package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestCapacityScopedContinueCannotOverrideTerminal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, _, _, _, _ := capacityFixture(t)
				ids := registerOverloadAuths(t, m, 2)
				for _, id := range ids {
					a, _ := m.GetByID(id)
					a.Metadata = map[string]any{"request_scoped_errors": []config.RequestScopedErrorRule{{Status: 429, Match: []string{"turn_rate_limit_exceeded"}, Action: "continue"}}}
					if _, err := m.Update(context.Background(), a); err != nil {
						t.Fatal(err)
					}
				}
				calls := 0
				failure := fixtureCapacityError{delay: 120 * time.Second, safe: false}
				m.RegisterExecutor(&customStreamMockExecutor{mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
					calls++
					return cliproxyexecutor.Response{}, failure
				}}, identifier: "codex", streamFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					calls++
					return nil, failure
				}})
				opts := bindingOptions("review-session", "review-caller", `{"input":"hello"}`)
				req := cliproxyexecutor.Request{Model: "gpt-5.6-terra", Payload: opts.OriginalRequest}
				var err error
				if stream {
					_, err = m.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
				} else {
					_, err = m.Execute(context.Background(), []string{"codex"}, req, opts)
				}
				if calls != 1 || !isCapacityFinal(err) {
					t.Fatalf("scoped continue bypassed replay guard: calls=%d err=%v", calls, err)
				}
			})
		})
	}
}

type oversizedCapacityError struct {
	fixtureCapacityError
	body    string
	headers http.Header
}

func (e oversizedCapacityError) Error() string        { return e.body }
func (e oversizedCapacityError) Headers() http.Header { return e.headers }

func TestCapacityGateRetainedErrorsBoundedOnReplacement(t *testing.T) {
	c := newCapacityCoordinator()
	small := fixtureCapacityError{delay: time.Minute, safe: true}
	c.pressure("account:model", small, false)
	huge := oversizedCapacityError{fixtureCapacityError: small, body: strings.Repeat("x", 65<<10)}
	if c.pressure("account:model", huge, false) != nil {
		t.Fatal("oversized replacement accepted")
	}
	if err, _ := c.remaining("account:model"); len(err.Error()) > 64<<10 {
		t.Fatal("retained oversized error")
	}
	// Header maps must not survive in shared admission state at all.
	c.pressure("account:headers", oversizedCapacityError{fixtureCapacityError: small, body: small.Error(), headers: http.Header{"X-Large": {strings.Repeat("x", 2<<20)}}}, false)
	err, _ := c.remaining("account:headers")
	var headers interface{ Headers() http.Header }
	if errors.As(err, &headers) && headers.Headers().Get("X-Large") != "" {
		t.Fatal("retained unaccounted upstream headers")
	}
}

func TestCapacityWaiterExpiryPreservesOriginalError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCapacityCoordinator()
		original := fixtureCapacityError{delay: time.Minute, safe: true}
		c.pressure("account:model", original, false)
		// A successful shared probe clears last; another admitted probe remains busy
		// until this older request's budget expires.
		c.complete("account:model")
		c.gates["account:model"].probing = true
		err := c.acquire(context.Background(), "account:model", time.Now().Add(time.Minute), original)
		if err == nil || statusCodeFromError(err) != 429 || !strings.Contains(err.Error(), "turn_rate_limit_exceeded") {
			t.Fatalf("lost original error: %T %v", err, err)
		}
	})
}

func TestCapacityReloadDoesNotEvictAdmittedWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, ctx, req, opts := capacityFixture(t)
		done := make(chan error, 1)
		calls := 0
		go func() {
			_, err := runCapacityRecovery(ctx, m, a, req, opts, func(context.Context, *Auth) (int, error) {
				calls++
				if calls == 1 {
					return 0, fixtureCapacityError{delay: time.Minute, safe: true}
				}
				return 1, nil
			})
			done <- err
		}()
		synctest.Wait()
		if m.CapacityRecoveryStatus().Pending != 1 {
			t.Fatal("not admitted")
		}
		cfg := &config.Config{}
		m.SetConfig(cfg)
		time.Sleep(time.Minute)
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if calls != 2 || m.CapacityRecoveryStatus().Pending != 0 {
			t.Fatal("reload broke admitted request")
		}
	})
}

func TestCapacityCommonAttemptReacquiresLease(t *testing.T) {
	m, a, ctx, req, opts := capacityFixture(t)
	opts, lease := m.trackPolicyRequest(ctx, opts)
	defer lease.release()
	lease.selectAuth(a.ID)
	finishPolicyAttempt(opts)
	// This is the state after pressure disappeared while the previous attempt
	// was being released. Every execution path must reenter attempt accounting.
	_, err := runCapacityRecovery(ctx, m, a, req, opts, func(context.Context, *Auth) (int, error) {
		if m.PolicyRuntime(a.ID).ActiveRequests != 1 {
			t.Error("upstream attempt appears idle")
		}
		return 1, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
