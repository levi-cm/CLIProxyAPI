package auth

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

type capacityRequestKey struct{}
type capacityRequest struct {
	config   config.CodexCapacityRetryConfig
	deadline time.Time
	attempts int
	last     error
}

// Limits are captured once per request. Reloads affect admission of new waits;
// reservations that already exist keep their cancellation and completion paths.
func (m *Manager) withCapacityRecovery(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := m.runtimeConfigSnapshot().Codex.CapacityRetry.WithDefaults()
	if !cfg.Enabled || cfg.Validate() != nil || m.HomeEnabled() {
		return ctx
	}
	return context.WithValue(ctx, capacityRequestKey{}, &capacityRequest{config: cfg, deadline: time.Now().Add(cfg.Budget())})
}

// capacityFinalError prevents the general retry rounds and credential/model
// fallback from multiplying the recovery budget. Its cause retains wire details.
type capacityFinalError struct {
	cause error
	until time.Time
}

func (e *capacityFinalError) Error() string {
	if e.cause == nil {
		return "capacity recovery budget exhausted"
	}
	return e.cause.Error()
}
func (e *capacityFinalError) Unwrap() error    { return e.cause }
func (e *capacityFinalError) StatusCode() int  { return statusCodeFromError(e.cause) }
func (e *capacityFinalError) IsCapacity() bool { return isCapacityError(e.cause) }
func (e *capacityFinalError) RetryAfter() *time.Duration {
	if e.until.IsZero() {
		return retryAfterFromError(e.cause)
	}
	delay := time.Until(e.until)
	if delay < 0 {
		delay = 0
	}
	return &delay
}
func isCapacityFinal(err error) bool { var final *capacityFinalError; return errors.As(err, &final) }

func capacityAttemptAllowed(ctx context.Context) error {
	state, _ := ctx.Value(capacityRequestKey{}).(*capacityRequest)
	if state == nil {
		return nil
	}
	if state.attempts >= state.config.MaxAttempts || !time.Now().Before(state.deadline) {
		if state.last != nil {
			return &capacityFinalError{cause: state.last}
		}
		return &capacityFinalError{cause: &Error{Code: ErrorCodeRequestScoped, Message: "request recovery budget exhausted", HTTPStatus: http.StatusTooManyRequests}}
	}
	state.attempts++
	return nil
}

type capacityGate struct {
	until    time.Time
	last     error
	probing  bool
	changed  chan struct{}
	failures int
	touched  time.Time
}
type capacityCoordinator struct {
	mu              sync.Mutex
	gates           map[string]*capacityGate
	pending         int
	pendingBytes    int64
	accounts        map[string]int
	sessions        map[string]bool
	availableMemory func() (int64, error)
}

func newCapacityCoordinator() *capacityCoordinator {
	return &capacityCoordinator{gates: make(map[string]*capacityGate), accounts: make(map[string]int), sessions: make(map[string]bool), availableMemory: capacityAvailableMemory}
}

type capacityReservation struct {
	owner            *capacityCoordinator
	account, session string
	bytes            int64
	once             sync.Once
}

func (r *capacityReservation) release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		c := r.owner
		c.mu.Lock()
		defer c.mu.Unlock()
		c.pending--
		c.pendingBytes -= r.bytes
		c.accounts[r.account]--
		if c.accounts[r.account] == 0 {
			delete(c.accounts, r.account)
		}
		delete(c.sessions, r.session)
	})
}
func (c *capacityCoordinator) reserve(cfg config.CodexCapacityRetryConfig, account, session string, bytes int64) *capacityReservation {
	available, err := c.availableMemory()
	if err != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if bytes <= 0 || bytes > cfg.MaxPendingBytes-c.pendingBytes || available < cfg.MinAvailableMemoryBytes || bytes > available-cfg.MinAvailableMemoryBytes || c.pending >= cfg.MaxPending || c.accounts[account] >= cfg.MaxPendingPerAccount || c.sessions[session] {
		return nil
	}
	c.pending++
	c.pendingBytes += bytes
	c.accounts[account]++
	c.sessions[session] = true
	return &capacityReservation{owner: c, account: account, session: session, bytes: bytes}
}

func (c *capacityCoordinator) pressure(key string, err error, probe bool) *capacityGate {
	// Validate every replacement, and retain only a bounded body/status/deadline
	// snapshot. Wrappers can carry large headers or request references.
	body := err.Error()
	if len(body) > 64<<10 {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	gate := c.gates[key]
	if gate == nil {
		// Bound retained errors even when admission is refused. Expired, inactive
		// entries can be discarded; active probes are never evicted.
		if len(c.gates) >= 256 {
			for k, g := range c.gates {
				if !g.probing && !g.until.After(now) && c.accounts[strings.SplitN(k, ":", 2)[0]] == 0 {
					delete(c.gates, k)
				}
			}
		}
		if len(c.gates) >= 256 || len(err.Error()) > 64<<10 {
			return nil
		}
		gate = &capacityGate{changed: make(chan struct{})}
		c.gates[key] = gate
	}
	gate.failures++
	gate.touched = now
	delay := capacityFallbackDelay(gate.failures)
	if hint := retryAfterFromError(err); hint != nil {
		delay = *hint
	}
	until := now.Add(delay)
	var deadline interface{ RetryDeadline() time.Time }
	if errors.As(err, &deadline) && !deadline.RetryDeadline().IsZero() {
		until = deadline.RetryDeadline()
	}
	if until.After(gate.until) {
		gate.until = until
	}
	gate.last = &capacityErrorSnapshot{body: strings.Clone(body), status: statusCodeFromError(err), until: gate.until}
	if probe {
		gate.probing = false
	}
	close(gate.changed)
	gate.changed = make(chan struct{})
	return gate
}
func capacityFallbackDelay(failures int) time.Duration {
	delay := 30 * time.Second
	for i := 1; i < failures && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	// Jitter is only positive so it cannot undercut an upstream deadline.
	return min(5*time.Minute, delay+time.Duration(rand.Int64N(int64(delay/20)+1)))
}
func (c *capacityCoordinator) existing(key string) *capacityGate {
	c.mu.Lock()
	defer c.mu.Unlock()
	g := c.gates[key]
	if g != nil && g.last == nil {
		return nil
	}
	return g
}
func (c *capacityCoordinator) remaining(key string) (error, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if g := c.gates[key]; g != nil {
		return g.last, g.until
	}
	return nil, time.Time{}
}
func (c *capacityCoordinator) complete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if g := c.gates[key]; g != nil {
		g.probing = false
		g.until = time.Time{}
		g.last = nil
		g.failures = 0
		close(g.changed)
		g.changed = make(chan struct{})
	}
}
func (c *capacityCoordinator) abandon(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if g := c.gates[key]; g != nil && g.probing {
		g.probing = false
		close(g.changed)
		g.changed = make(chan struct{})
	}
}
func (c *capacityCoordinator) acquire(ctx context.Context, key string, budget time.Time, fallback error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		g := c.gates[key]
		if g == nil {
			c.mu.Unlock()
			return nil
		}
		now := time.Now()
		until, changed := g.until, g.changed
		if !now.Before(budget) || !until.Before(budget) {
			last := g.last
			if last == nil {
				last = fallback
			}
			c.mu.Unlock()
			return &capacityFinalError{cause: last, until: until}
		}
		if !g.probing && !until.After(now) {
			g.probing = true
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()
		wait := time.Until(budget)
		if until.After(now) && until.Sub(now) < wait {
			wait = until.Sub(now)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// chargeCapacityPayload deliberately overcharges retained buffers and includes
// a MiB for request/transport objects. It takes no copy of the payload to wait.
func chargeCapacityPayload(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) int64 {
	charge := int64(1<<20) + 4*(int64(cap(req.Payload))+int64(cap(opts.OriginalRequest)))
	for key, values := range opts.Headers {
		charge += int64(len(key))
		for _, v := range values {
			charge += 2 * int64(len(v))
		}
	}
	return charge
}
func capacityRecoveryIdentity(a *Auth) string {
	return policyDiagnosticHash(a.ID + "\x00" + policyClientIdentity(a))
}
func capacityRecoverySession(opts cliproxyexecutor.Options, model string) string {
	primary, _ := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primary == "" {
		return ""
	}
	return policyDiagnosticHash(policyClientScope(opts) + "\x00" + primary + "\x00" + canonicalModelKey(model))
}
func (m *Manager) revalidateCapacityAuth(original *Auth, model string) (*Auth, error) {
	fresh, ok := m.GetByID(original.ID)
	if !ok || fresh.Disabled || fresh.Status == StatusDisabled || fresh.RegistrationEpoch != original.RegistrationEpoch || policyClientIdentity(fresh) != policyClientIdentity(original) {
		return nil, fmt.Errorf("capacity recovery credential identity or eligibility changed")
	}
	if blocked, _, _ := isAuthBlockedForModel(fresh, model, time.Now()); blocked {
		return nil, fmt.Errorf("capacity recovery credential is no longer eligible")
	}
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	if policy, ok := selector.(*EarliestDeadlineSelector); ok && policy.policy.Settings().Enabled {
		eligible, _, _ := policy.evaluate("codex", model, []*Auth{fresh})
		if len(eligible) != 1 {
			return nil, fmt.Errorf("capacity recovery account no longer satisfies policy")
		}
	}
	return fresh, nil
}

// runCapacityRecovery only retries explicit pre-output admission failures on the
// same logical credential/model. The closure returns only after its rejected
// upstream connection is released; no network timeout is introduced here.
func runCapacityRecovery[T any](ctx context.Context, m *Manager, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, attempt func(context.Context, *Auth) (T, error)) (out T, err error) {
	state, _ := ctx.Value(capacityRequestKey{}).(*capacityRequest)
	if state == nil || auth.Provider != "codex" {
		return attempt(ctx, auth)
	}
	account := capacityRecoveryIdentity(auth)
	key := account + ":" + policyDiagnosticHash(canonicalModelKey(req.Model))
	var reservation *capacityReservation
	defer func() { reservation.release() }()
	probing := false
	defer func() {
		if probing {
			m.capacity.abandon(key)
		}
	}()
	waiting := m.capacity.existing(key) != nil
	current := auth
	for {
		if waiting {
			finishPolicyAttempt(opts)
			last, until := m.capacity.remaining(key)
			if last == nil && state.last == nil {
				waiting = false
				continue
			}
			if last != nil {
				state.last = last
			}
			if state.attempts >= state.config.MaxAttempts || !until.Before(state.deadline) {
				return out, &capacityFinalError{cause: state.last, until: until}
			}
			if reservation == nil {
				session := capacityRecoverySession(opts, req.Model)
				if session == "" {
					return out, &capacityFinalError{cause: state.last, until: until}
				}
				// A reload can tighten admission without evicting existing requests.
				limits := m.runtimeConfigSnapshot().Codex.CapacityRetry.WithDefaults()
				if !limits.Enabled || limits.Validate() != nil {
					return out, &capacityFinalError{cause: state.last, until: until}
				}
				reservation = m.capacity.reserve(limits, account, session, chargeCapacityPayload(req, opts))
				if reservation == nil {
					return out, &capacityFinalError{cause: state.last, until: until}
				}
			}
			if err = m.capacity.acquire(ctx, key, state.deadline, state.last); err != nil {
				return out, err
			}
			probing = true
		}
		current, err = m.revalidateCapacityAuth(auth, req.Model)
		if err != nil {
			if state.last != nil {
				return out, &capacityFinalError{cause: state.last}
			}
			return out, &capacityFinalError{cause: &Error{Code: ErrorCodeRequestScoped, Message: err.Error(), HTTPStatus: http.StatusServiceUnavailable}}
		}
		if err = beginPolicyAttempt(ctx, opts, current.ID); err != nil {
			return out, err
		}

		if err = ctx.Err(); err != nil {
			return out, err
		}
		if err = capacityAttemptAllowed(ctx); err != nil {
			return out, err
		}
		out, err = attempt(ctx, current)
		if !isCapacityError(err) {
			if probing {
				m.capacity.complete(key)
				probing = false
			}
			return out, err
		}
		state.last = err
		var safe interface{ CapacityRetrySafe() bool }
		if !errors.As(err, &safe) || !safe.CapacityRetrySafe() {
			return out, &capacityFinalError{cause: err}
		}
		gate := m.capacity.pressure(key, err, probing)
		probing = false
		if gate == nil {
			return out, &capacityFinalError{cause: err}
		}
		log.WithFields(log.Fields{"component": "codex-capacity", "classification": ErrorCodeCapacity, "account_hash": account, "model_hash": policyDiagnosticHash(canonicalModelKey(req.Model)), "attempt": state.attempts}).Info("upstream admission pressure")
		waiting = true
	}
}

func capacityRecoveryActive(ctx context.Context) bool {
	state, _ := ctx.Value(capacityRequestKey{}).(*capacityRequest)
	return state != nil
}

// CapacityRecoveryStatus is transient admission accounting, never durable quota.
type CapacityRecoveryStatus struct {
	Pending      int   `json:"pending"`
	PendingBytes int64 `json:"pending_bytes"`
}

func (m *Manager) CapacityRecoveryStatus() CapacityRecoveryStatus {
	m.capacity.mu.Lock()
	defer m.capacity.mu.Unlock()
	return CapacityRecoveryStatus{Pending: m.capacity.pending, PendingBytes: m.capacity.pendingBytes}
}

// capacityErrorSnapshot intentionally retains no upstream header map or cause.
type capacityErrorSnapshot struct {
	body   string
	status int
	until  time.Time
}

func (e *capacityErrorSnapshot) Error() string            { return e.body }
func (e *capacityErrorSnapshot) StatusCode() int          { return e.status }
func (e *capacityErrorSnapshot) IsCapacity() bool         { return true }
func (e *capacityErrorSnapshot) RetryDeadline() time.Time { return e.until }
func (e *capacityErrorSnapshot) RetryAfter() *time.Duration {
	d := max(time.Duration(0), time.Until(e.until))
	return &d
}
func (e *capacityFinalError) Headers() http.Header { return SafeResponseHeaders(e) }
