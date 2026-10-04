package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
)

// PolicyRuntimeStatus exposes local execution accounting. UpstreamTransport is
// unknown unless an executor explicitly reports its actual selected transport.
type PolicyRuntimeStatus struct {
	ActiveRequests int        `json:"active_requests"`
	ActiveBindings int        `json:"active_bindings"`
	Transport      string     `json:"transport"`
	LastSelectedAt *time.Time `json:"last_selected_at"`
}

type policyRequestLease struct {
	mu             sync.Mutex
	manager        *Manager
	authID         string
	transport      string
	released       bool
	ctx            context.Context
	streaming      bool
	refreshPending bool
}

type policyRequestDemand struct {
	providers []string
	model     string
}

const policyRequestLeaseMetadataKey = "account_policy_request_lease"

type policyRequestLeaseKey struct{}

func (m *Manager) trackPolicyRequest(ctx context.Context, opts cliproxyexecutor.Options, demands ...policyRequestDemand) (cliproxyexecutor.Options, *policyRequestLease) {
	if ctx == nil {
		ctx = context.Background()
	}
	lease := &policyRequestLease{manager: m, ctx: ctx, transport: "downstream_http;upstream_unknown"}
	if cliproxyexecutor.DownstreamWebsocket(ctx) {
		lease.transport = "downstream_websocket;upstream_unknown"
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		meta[key] = value
	}
	callback, _ := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string))
	meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey] = func(id string) {
		lease.selectAuth(id)
		if callback != nil {
			callback(id)
		}
	}
	meta[policyRequestLeaseMetadataKey] = lease
	if len(demands) > 0 {
		m.policyRuntimeMu.Lock()
		if m.policyPending == nil {
			m.policyPending = make(map[*policyRequestLease]policyRequestDemand)
		}
		m.policyPending[lease] = demands[0]
		m.policyRuntimeMu.Unlock()
	}
	opts.Metadata = meta
	return opts, lease
}

func (l *policyRequestLease) selectAuth(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.authID == id {
		return
	}
	for {
		l.manager.policyRuntimeMu.Lock()
		reservation := l.manager.policyResetReservations[id]
		if reservation == nil {
			break
		}
		l.manager.policyRuntimeMu.Unlock()
		select {
		case <-l.ctx.Done():
			return
		case <-reservation:
		}
	}
	defer l.manager.policyRuntimeMu.Unlock()
	if l.manager.policyRuntime == nil {
		l.manager.policyRuntime = make(map[string]PolicyRuntimeStatus)
	}
	if l.authID != "" {
		status := l.manager.policyRuntime[l.authID]
		if status.ActiveRequests > 0 {
			status.ActiveRequests--
		}
		l.manager.policyRuntime[l.authID] = status
	}
	l.authID = id
	if id != "" {
		status := l.manager.policyRuntime[id]
		status.ActiveRequests++
		status.Transport = l.transport
		selectedAt := time.Now().UTC()
		status.LastSelectedAt = &selectedAt
		l.manager.policyRuntime[id] = status
	}
}

func (l *policyRequestLease) finishAttempt() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.streaming {
		return
	}
	l.manager.policyRuntimeMu.Lock()
	defer l.manager.policyRuntimeMu.Unlock()
	status := l.manager.policyRuntime[l.authID]
	if status.ActiveRequests > 0 {
		status.ActiveRequests--
	}
	if l.authID != "" {
		l.manager.policyRuntime[l.authID] = status
	}
	l.authID = ""
}

func finishPolicyAttempt(opts cliproxyexecutor.Options) {
	if lease, ok := opts.Metadata[policyRequestLeaseMetadataKey].(*policyRequestLease); ok {
		lease.finishAttempt()
	}
}

func beginPolicyAttempt(ctx context.Context, opts cliproxyexecutor.Options, authID string) error {
	if lease, ok := opts.Metadata[policyRequestLeaseMetadataKey].(*policyRequestLease); ok {
		lease.selectAuth(authID)
		return ctx.Err()
	}
	return nil
}

func (l *policyRequestLease) release() {
	if l == nil {
		return
	}
	refreshPending := false
	refreshID := ""
	defer func() {
		if refreshPending {
			l.manager.requestPolicyRefresh(refreshID)
		}
	}()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	refreshPending = l.refreshPending
	refreshID = l.authID
	l.manager.policyRuntimeMu.Lock()
	defer l.manager.policyRuntimeMu.Unlock()
	delete(l.manager.policyPending, l)
	status := l.manager.policyRuntime[l.authID]
	if status.ActiveRequests > 0 {
		status.ActiveRequests--
	}
	if l.authID != "" {
		l.manager.policyRuntime[l.authID] = status
	}
}

func (m *Manager) PolicyRuntime(id string) PolicyRuntimeStatus {
	if m == nil {
		return PolicyRuntimeStatus{Transport: "unknown"}
	}
	m.policyRuntimeMu.Lock()
	status := m.policyRuntime[id]
	if status.LastSelectedAt != nil {
		selectedAt := *status.LastSelectedAt
		status.LastSelectedAt = &selectedAt
	}
	m.policyRuntimeMu.Unlock()
	if status.Transport == "" {
		status.Transport = "unknown"
	}
	if observer, ok := m.Selector().(interface{ ActiveBindings(string) int }); ok {
		status.ActiveBindings = observer.ActiveBindings(id)
	}
	return status
}

// PolicyHasDemand reports requests currently selected on an account, including
// requests waiting for an eligible retry. It does not invent queued workload.
func (m *Manager) PolicyHasDemand(id string) bool {
	if m == nil {
		return false
	}
	a, ok := m.GetByID(id)
	if !ok {
		return false
	}
	m.policyRuntimeMu.Lock()
	demands := make([]policyRequestDemand, 0, len(m.policyPending))
	for _, demand := range m.policyPending {
		demands = append(demands, demand)
	}
	active := m.policyRuntime[id].ActiveRequests
	m.policyRuntimeMu.Unlock()
	if active > 0 {
		return true
	}
	for _, demand := range demands {
		providerAllowed := false
		for _, provider := range demand.providers {
			if provider == a.Provider {
				providerAllowed = true
				break
			}
		}
		if providerAllowed && m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, demand.model) {
			return true
		}
	}
	return false
}
func (m *Manager) PolicyIsIdle(id string) bool { return m.PolicyRuntime(id).ActiveRequests == 0 }

// AcquirePolicyReset atomically reserves an idle credential. New requests wait
// at selection until the reset's read/write/reconciliation critical section ends.
func (m *Manager) AcquirePolicyReset(ctx context.Context, id string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil || id == "" {
		return nil, &accountpolicy.Error{Code: "account_not_found", Message: "account is required"}
	}
	m.policyRuntimeMu.Lock()
	defer m.policyRuntimeMu.Unlock()
	if m.policyRuntime[id].ActiveRequests > 0 || m.policyResetReservations[id] != nil {
		return nil, &accountpolicy.Error{Code: "account_busy", Message: "account has active inference or reset"}
	}
	if m.policyResetReservations == nil {
		m.policyResetReservations = make(map[string]chan struct{})
	}
	done := make(chan struct{})
	m.policyResetReservations[id] = done
	var once sync.Once
	return func() {
		once.Do(func() {
			m.policyRuntimeMu.Lock()
			delete(m.policyResetReservations, id)
			close(done)
			m.policyRuntimeMu.Unlock()
		})
	}, nil
}

// CredentialRoundTripper reuses the host's credential-specific transport.
func (m *Manager) CredentialRoundTripper(id string) http.RoundTripper {
	if m == nil {
		return nil
	}
	a, ok := m.GetByID(id)
	if !ok {
		return nil
	}
	if strings.TrimSpace(a.ProxyURL) == "" {
		if cfg := m.runtimeConfigSnapshot(); cfg != nil {
			a.ProxyURL = cfg.ProxyURL
		}
	}
	if rt := m.roundTripperFor(a); rt != nil {
		return rt
	}
	if strings.TrimSpace(a.ProxyURL) != "" {
		transport, _, err := proxyutil.BuildHTTPTransport(a.ProxyURL)
		if err != nil {
			return policyInvalidProxyTransport{}
		}
		return transport
	}
	return nil
}

type policyInvalidProxyTransport struct{}

// PolicyModelForAuth resolves the same quota key used by normal routing.
func (m *Manager) PolicyModelForAuth(auth *Auth, model string) string {
	return m.selectionModelForAuth(auth, model)
}

func (policyInvalidProxyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &accountpolicy.Error{Code: "invalid_proxy", Message: "configured credential proxy is invalid"}
}
