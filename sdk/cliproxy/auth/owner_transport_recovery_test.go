package auth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"golang.org/x/net/http2"
)

func TestHTTP2StreamFailureDoesNotCoolOwner(t *testing.T) {
	for _, failure := range []error{
		http2.StreamError{StreamID: 31, Code: http2.ErrCodeInternal},
		errors.New("stream error: stream ID 31; INTERNAL_ERROR; received from peer"),
	} {
		t.Run(failure.Error(), func(t *testing.T) {
			if !isTransientTransportError(failure) {
				t.Fatal("HTTP/2 stream failure lost transport classification")
			}
			result := resultErrorFromError(failure)
			if !shouldSkipCredentialCooldown(result) {
				t.Fatal("temporary stream failure would poison owner cooldown")
			}
			if isTransientTransportError(&Error{HTTPStatus: 429, Message: failure.Error()}) {
				t.Fatal("HTTP status must not be reclassified from body text")
			}
		})
	}
}

type ownerTransportExecutor struct {
	transportThenSuccessExecutor
	owners      []string
	failure     error
	always      bool
	afterOutput bool
	bootstrap   bool
}

func (e *ownerTransportExecutor) Execute(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.owners = append(e.owners, a.ID)
	if len(e.owners) == 1 || e.always {
		return cliproxyexecutor.Response{}, e.failure
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}
func (e *ownerTransportExecutor) ExecuteStream(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.owners = append(e.owners, a.ID)
	fail := len(e.owners) == 1 || e.always
	if fail && !e.afterOutput && !e.bootstrap {
		return nil, e.failure
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	if !fail || e.afterOutput {
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: output\n\n")}
	}
	if fail {
		chunks <- cliproxyexecutor.StreamChunk{Err: e.failure}
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}
func ownerTransportFixture(t *testing.T, e *ownerTransportExecutor) (*Manager, cliproxyexecutor.Options) {
	t.Helper()
	s, p, auths, now := durableBindingFixture(t)
	m := NewManager(nil, s, nil)
	m.SetRetryConfig(1, 0, 0)
	e.identifier = "codex"
	m.RegisterExecutor(e)
	for i, a := range auths {
		registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
		registered, err := m.Register(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		auths[i] = registered
	}
	s.SetBindingAuthResolver(m.GetByID)
	successfulBinding(t, s, auths, bindingOptions("transport-owned", "caller", `{"input":"hello"}`))
	addExpiringCredit(p, now)
	opts := bindingOptions("transport-owned", "caller", `{"input":[{"type":"reasoning","encrypted_content":"fixture-opaque"},{"role":"user","content":"continue"}]}`)
	return m, opts
}
func TestEncryptedOwnerTransportRecovery(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "bootstrap", "after-output", "exhausted"} {
		t.Run(mode, func(t *testing.T) {
			failure := http2.StreamError{StreamID: 31, Code: http2.ErrCodeInternal}
			e := &ownerTransportExecutor{failure: failure, afterOutput: mode == "after-output", always: mode == "exhausted", bootstrap: mode == "bootstrap"}
			m, opts := ownerTransportFixture(t, e)
			req := cliproxyexecutor.Request{Model: "gpt-5", Payload: opts.OriginalRequest}
			var err error
			if mode == "execute" || mode == "exhausted" {
				_, err = m.Execute(context.Background(), []string{"codex"}, req, opts)
			} else {
				var stream *cliproxyexecutor.StreamResult
				stream, err = m.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
				if err == nil {
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			}
			if mode == "after-output" || mode == "exhausted" {
				if err == nil {
					t.Fatal("transport failure was lost")
				}
				if statusCodeFromError(err) == http.StatusConflict {
					t.Fatalf("transport failure became ownership 409: %v", err)
				}
			} else if err != nil {
				t.Fatalf("same-owner recovery failed: %v", err)
			}
			want := []string{"a", "a"}
			if mode == "after-output" {
				want = []string{"a"}
			}
			if !reflect.DeepEqual(e.owners, want) {
				t.Fatalf("owners=%v want=%v", e.owners, want)
			}
			assertNoCooldown(t, m, "a", "gpt-5")
			if mode == "after-output" {
				_, err = m.Execute(context.Background(), []string{"codex"}, req, opts)
				if err != nil || e.owners[len(e.owners)-1] != "a" {
					t.Fatalf("next continuation lost owner: %v owners=%v", err, e.owners)
				}
			}
		})
	}
}

func TestOwnerTransportExhaustionReturnsRetryableGatewayError(t *testing.T) {
	e := &ownerTransportExecutor{failure: http2.StreamError{StreamID: 31, Code: http2.ErrCodeInternal}, always: true}
	m, opts := ownerTransportFixture(t, e)
	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5", Payload: opts.OriginalRequest}, opts)
	if statusCodeFromError(err) != http.StatusBadGateway {
		t.Fatalf("status=%d err=%v; want retryable 502", statusCodeFromError(err), err)
	}
	if !errors.Is(err, e.failure) {
		t.Fatal("transport cause was lost")
	}
}

func TestEncryptedOwner503DoesNotBecomeOwnershipConflict(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		e := &ownerTransportExecutor{failure: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "no healthy upstream"}, always: true}
		m, opts := ownerTransportFixture(t, e)
		req := cliproxyexecutor.Request{Model: "gpt-5", Payload: opts.OriginalRequest}
		var err error
		if streaming {
			_, err = m.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
		} else {
			_, err = m.Execute(context.Background(), []string{"codex"}, req, opts)
		}
		if statusCodeFromError(err) != http.StatusServiceUnavailable {
			t.Fatalf("streaming=%v temporary 503 became status=%d: %v", streaming, statusCodeFromError(err), err)
		}
		if len(e.owners) == 0 {
			t.Fatal("original owner was never attempted")
		}
		for _, owner := range e.owners {
			if owner != "a" {
				t.Fatalf("temporary 503 moved encrypted conversation: %v", e.owners)
			}
		}
	}
}
func TestLegacyOwnerTransportCooldownReturnsRetryableError(t *testing.T) {
	for _, failure := range []*Error{
		{Message: "stream error: stream ID 31; INTERNAL_ERROR; received from peer"},
		{HTTPStatus: http.StatusServiceUnavailable, Message: "no healthy upstream"},
		{HTTPStatus: http.StatusBadGateway, Message: "upstream unavailable"},
	} {
		t.Run(failure.Message, func(t *testing.T) {
			s, _, auths, now := durableBindingFixture(t)
			successfulBinding(t, s, auths, bindingOptions("legacy-owner", "caller", `{"input":"hello"}`))
			auths[0].ModelStates = map[string]*ModelState{"gpt-5": {Unavailable: true, Status: StatusError, NextRetryAfter: now.Add(time.Minute), LastError: failure}}
			opts := bindingOptions("legacy-owner", "caller", `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`)
			a, err := s.Pick(context.Background(), "codex", "gpt-5", opts, auths)
			if a != nil || statusCodeFromError(err) != http.StatusServiceUnavailable {
				t.Fatalf("owner=%v err=%v; want retryable 503", a, err)
			}
			if got := SafeResponseHeaders(err).Get("Retry-After"); got != "60" {
				t.Fatalf("Retry-After=%q want 60", got)
			}
			if !auths[0].ModelStates["gpt-5"].NextRetryAfter.Equal(now.Add(time.Minute)) {
				t.Fatal("cooldown was mutated")
			}
		})
	}
}

func TestTemporaryOwnerRecoveryDoesNotBypassHardBlocks(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests} {
		if isOwnerTemporaryFailure(&Error{HTTPStatus: status, Message: "no healthy upstream"}) {
			t.Fatalf("hard status %d classified from response text", status)
		}
	}
	s, _, auths, now := durableBindingFixture(t)
	for _, quota := range []bool{false, true} {
		a := auths[0].Clone()
		a.Disabled = !quota
		a.Quota.Exceeded = quota
		a.NextRetryAfter = now.Add(time.Minute)
		a.LastError = &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "no healthy upstream"}
		if s.ownerTransportCooldown(a, "gpt-5") != nil {
			t.Fatal("disabled or quota-blocked owner weakened")
		}
	}
}
