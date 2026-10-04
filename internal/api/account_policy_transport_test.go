package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// These tests use the public API, real local sockets, and the production Codex
// executors. No credential file, provider endpoint, discovery call, or reset
// write is involved. The policy clock is frozen independently of socket I/O.
type policyTransportSource struct {
	mu        sync.Mutex
	settings  accountpolicy.Settings
	snapshots map[string]accountpolicy.Snapshot
	decisions []accountpolicy.Decision
}

func (s *policyTransportSource) Settings() accountpolicy.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	settings := s.settings
	settings.Accounts = maps.Clone(settings.Accounts)
	settings.CreditTypes = slices.Clone(settings.CreditTypes)
	return settings
}
func (s *policyTransportSource) Snapshot(id string) (accountpolicy.Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.snapshots[id]
	snapshot.Buckets = slices.Clone(snapshot.Buckets)
	snapshot.Credits = slices.Clone(snapshot.Credits)
	return snapshot, ok
}
func (s *policyTransportSource) RecordDecision(d accountpolicy.Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisions = append(s.decisions, d)
}
func (s *policyTransportSource) update(id string, update func(*accountpolicy.Snapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := s.snapshots[id]
	snapshot.Buckets = slices.Clone(snapshot.Buckets)
	snapshot.Credits = slices.Clone(snapshot.Credits)
	update(&snapshot)
	s.snapshots[id] = snapshot
}

type policyTransportObservation struct {
	account   string
	transport string
	path      string
	payload   []byte
}

type policyTransportFixture struct {
	mu           sync.Mutex
	observations []policyTransportObservation
	upgrades     int
	rejectWS     bool
	pause        <-chan struct{}
}

func (f *policyTransportFixture) captured() ([]policyTransportObservation, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]policyTransportObservation(nil), f.observations...), f.upgrades
}

func (f *policyTransportFixture) record(account, transport, path string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observations = append(f.observations, policyTransportObservation{account, transport, path, append([]byte(nil), body...)})
}

func policyTransportEvents(account string, payload []byte) []string {
	completed := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%s","object":"response","status":"completed","model":"gpt-5-codex","output":[{"id":"msg_fixture","type":"message","role":"assistant","content":[{"type":"output_text","text":"fixture %s"}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`, account, account)
	if gjson.GetBytes(payload, `tools.#(name=="fixture_tool")`).Exists() {
		completed = fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%s","object":"response","status":"completed","model":"gpt-5-codex","output":[{"id":"fc_fixture","type":"function_call","call_id":"fixture-call","name":"fixture_tool","arguments":"{}","status":"completed"}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`, account)
	}
	return []string{
		fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_%s","status":"in_progress","output":[]}}`, account),
		fmt.Sprintf(`{"type":"response.output_text.delta","delta":"fixture %s"}`, account),
		completed,
	}
}

func (f *policyTransportFixture) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer fixture-")
	if account != "a" && account != "b" {
		t.Errorf("unexpected fixture authorization identity %q", account)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if websocket.IsWebSocketUpgrade(r) {
		if f.rejectWS {
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream websocket upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		f.mu.Lock()
		f.upgrades++
		f.mu.Unlock()
		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			f.record(account, "websocket", r.URL.Path, payload)
			for _, event := range policyTransportEvents(account, payload) {
				if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(event)); errWrite != nil {
					return
				}
			}
		}
	}
	body, errRead := io.ReadAll(r.Body)
	if errRead != nil {
		t.Errorf("read fixture request: %v", errRead)
		return
	}
	f.record(account, "http", r.URL.Path, body)
	if strings.HasSuffix(r.URL.Path, "/compact") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"compact_%s","object":"response.compaction","output":[],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`, account)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for i, event := range policyTransportEvents(account, body) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		w.(http.Flusher).Flush()
		// Emit the next event before pausing so the Responses SSE framer has
		// an unambiguous complete initial frame, even when an executor returns
		// individual data lines without blank-line delimiters.
		if i == 1 && f.pause != nil {
			select {
			case <-f.pause:
			case <-r.Context().Done():
				return
			}
		}
	}
}

type policyTransportHarness struct {
	url     string
	source  *policyTransportSource
	fixture *policyTransportFixture
	manager *auth.Manager
	a, b    string
	now     time.Time
	client  *http.Client
	dialer  *websocket.Dialer
}

func newPolicyTransportHarness(t *testing.T, fixture *policyTransportFixture) *policyTransportHarness {
	t.Helper()
	if fixture == nil {
		fixture = &policyTransportFixture{}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fixture.serve(t, w, r) }))
	t.Cleanup(upstream.Close)
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"test-key"}, DisableImageGeneration: config.DisableImageGenerationAll}, WebsocketAuth: true}
	cfg.Codex.DisableCodexCloaking = true
	cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gpt-5-codex"}}, Params: map[string]any{"instructions": "fixture final payload"}}}
	cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "gpt-5-codex"}}, Params: []string{"metadata.fixture_filter"}}}
	server := newTestServerWithConfig(t, cfg)
	manager := server.handlers.AuthManager
	manager.SetConfig(cfg)
	manager.RegisterExecutor(executor.NewCodexAutoExecutor(cfg))
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	settings := accountpolicy.DefaultSettings()
	settings.Enabled, settings.Automation = true, "auto_expiring"
	settings.Fallback = "fill-first"
	a, b := t.Name()+"-a", t.Name()+"-b"
	source := &policyTransportSource{settings: settings, snapshots: map[string]accountpolicy.Snapshot{}}
	for _, account := range []struct {
		id, alias, priority string
		days                int
	}{{a, "a", "20", 4}, {b, "b", "1", 7}} {
		credential := &auth.Auth{ID: account.id, Provider: "codex", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "fixture-" + account.alias, "base_url": upstream.URL, "websockets": "true", "priority": account.priority}}
		if _, err := manager.Register(context.Background(), credential); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(account.id, "codex", []*registry.ModelInfo{{ID: "gpt-5-codex"}})
		manager.RefreshSchedulerEntry(account.id)
		id := account.id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		allowed := true
		source.snapshots[account.id] = accountpolicy.Snapshot{Identity: accountpolicy.Identity{CredentialID: account.id, AccountID: "fixture-" + account.alias, WorkspaceID: "fixture-workspace-" + account.alias, Provider: "codex", Generation: 1}, Eligible: true, Status: "healthy", ObservedAt: now, InventoryObservedAt: now, InventoryComplete: true, Buckets: []accountpolicy.Bucket{
			{Scope: "ordinary", DurationSeconds: 18000, UsedPercent: 20, Allowed: &allowed, ResetAt: now.Add(time.Hour), ObservedAt: now},
			{Scope: "ordinary", DurationSeconds: 604800, UsedPercent: 25, Allowed: &allowed, ResetAt: now.Add(time.Duration(account.days) * 24 * time.Hour), ObservedAt: now},
		}}
	}
	expiry := now.Add(24 * time.Hour)
	source.update(b, func(s *accountpolicy.Snapshot) {
		s.AvailableCredits = 1
		s.Credits = []accountpolicy.Credit{{ID: "fixture-b-credit", Type: "codex_rate_limits", Status: "available", DetailsKnown: true, ExpiresAt: &expiry, Scopes: []string{"ordinary"}}}
	})
	selector := auth.NewEarliestDeadlineSelector(source, &auth.FillFirstSelector{}, func() time.Time { return now })
	selector.SetIdleCheck(manager.PolicyIsIdle)
	manager.SetSelector(selector)
	t.Cleanup(selector.Stop)
	downstream := httptest.NewUnstartedServer(server.engine)
	if address := os.Getenv("ACCOUNT_POLICY_TEST_LISTEN"); address != "" {
		host, _, errSplit := net.SplitHostPort(address)
		if errSplit != nil {
			t.Fatal(errSplit)
		}
		ip := net.ParseIP(host)
		_, tailnet, _ := net.ParseCIDR("100.64.0.0/10")
		if ip == nil || (!ip.IsLoopback() && !tailnet.Contains(ip)) {
			t.Fatalf("test listener must be loopback or a private tailnet IP, got %q", address)
		}
		listener, errListen := net.Listen("tcp", address)
		if errListen != nil {
			t.Fatal(errListen)
		}
		_ = downstream.Listener.Close()
		downstream.Listener = listener
	}
	downstream.Start()
	t.Cleanup(downstream.Close)
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	dialer := *websocket.DefaultDialer
	dialer.Proxy = nil
	return &policyTransportHarness{url: downstream.URL, source: source, fixture: fixture, manager: manager, a: a, b: b, now: now, client: &http.Client{Transport: transport}, dialer: &dialer}
}

const policyTransportInput = `"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"sanitized fixture"}],"metadata":{"fixture_filter":"remove"}`

func (h *policyTransportHarness) httpRequest(t *testing.T, path, body string, headers http.Header) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	response, errDo := h.client.Do(req)
	if errDo != nil {
		t.Fatal(errDo)
	}
	defer func() { _ = response.Body.Close() }()
	output, errRead := io.ReadAll(response.Body)
	if errRead != nil {
		t.Fatal(errRead)
	}
	return response.StatusCode, string(output)
}

func (h *policyTransportHarness) dial(t *testing.T, headers http.Header) *websocket.Conn {
	t.Helper()
	if headers == nil {
		headers = http.Header{}
	}
	headers.Set("Authorization", "Bearer test-key")
	conn, response, err := h.dialer.Dial(strings.Replace(h.url, "http://", "ws://", 1)+"/v1/responses", headers)
	if err != nil {
		if response != nil {
			t.Fatalf("downstream websocket: %v (status %d)", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func policyTransportTurn(t *testing.T, conn *websocket.Conn, payload string) string {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	for {
		_, output, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Fatal(errRead)
		}
		switch gjson.GetBytes(output, "type").String() {
		case "response.completed":
			return string(output)
		case "error", "response.failed":
			t.Fatalf("websocket fixture failed: %s", output)
		}
	}
}

func TestAccountPolicyRealTransportsApplySameEligibility(t *testing.T) {
	cases := []struct {
		name  string
		want  string
		setup func(*policyTransportHarness)
	}{
		{"expiring_credit_overrides_static_priority", "b", nil},
		{"natural_weekly_deadline", "a", func(h *policyTransportHarness) {
			h.source.update(h.b, func(s *accountpolicy.Snapshot) { s.Credits = nil; s.AvailableCredits = 0 })
		}},
		{"short_window_exhaustion", "a", func(h *policyTransportHarness) {
			h.source.update(h.b, func(s *accountpolicy.Snapshot) { s.Buckets[0].UsedPercent = 100 })
		}},
		{"weekly_exhaustion", "a", func(h *policyTransportHarness) {
			h.source.update(h.b, func(s *accountpolicy.Snapshot) { s.Buckets[1].UsedPercent = 100 })
		}},
		{"model_specific_block", "a", func(h *policyTransportHarness) {
			h.source.update(h.b, func(s *accountpolicy.Snapshot) {
				denied := false
				s.Buckets = append(s.Buckets, accountpolicy.Bucket{Scope: "model", Model: "gpt-5-codex", Allowed: &denied, ObservedAt: h.now})
			})
		}},
		{"unrelated_model_block_does_not_exclude_account", "b", func(h *policyTransportHarness) {
			h.source.update(h.b, func(s *accountpolicy.Snapshot) {
				denied := false
				s.Buckets = append(s.Buckets, accountpolicy.Bucket{Scope: "model", Model: "different-fixture-model", Allowed: &denied, ObservedAt: h.now})
			})
		}},
		{"manual_hold_excludes_urgent_account", "a", func(h *policyTransportHarness) {
			h.source.settings.Accounts[h.b] = accountpolicy.AccountControl{Hold: true}
		}},
		{"credential_disabled_excludes_urgent_account", "a", func(h *policyTransportHarness) {
			credential, ok := h.manager.GetByID(h.b)
			if !ok {
				t.Fatal("fixture credential disappeared")
			}
			credential.Disabled = true
			if _, err := h.manager.Update(context.Background(), credential); err != nil {
				t.Fatal(err)
			}
		}},
		{"count_without_details_does_not_invent_deadline", "a", func(h *policyTransportHarness) {
			h.source.update(h.b, func(s *accountpolicy.Snapshot) {
				s.Credits = nil
				s.AvailableCredits = 3
				s.InventoryComplete = false
			})
		}},
		{"nonexpiring_credit_keeps_natural_deadline", "a", func(h *policyTransportHarness) {
			h.source.update(h.b, func(s *accountpolicy.Snapshot) { s.Credits[0].ExpiresAt = nil })
		}},
		{"stale_evidence_uses_original_priority", "a", func(h *policyTransportHarness) {
			h.source.update(h.b, func(s *accountpolicy.Snapshot) { s.ObservedAt = h.now.Add(-121 * time.Second) })
		}},
		{"reserve_excludes_urgent_account", "a", func(h *policyTransportHarness) {
			h.source.settings.Accounts[h.b] = accountpolicy.AccountControl{ReservePercent: 80}
		}},
		{"disabled_restores_original_priority", "a", func(h *policyTransportHarness) { h.source.settings.Enabled = false }},
	}
	for _, tc := range cases {
		for _, transport := range []string{"http", "sse", "websocket"} {
			t.Run(tc.name+"/"+transport, func(t *testing.T) {
				h := newPolicyTransportHarness(t, nil)
				if tc.setup != nil {
					tc.setup(h)
				}
				var output string
				if transport == "websocket" {
					output = policyTransportTurn(t, h.dial(t, nil), `{"type":"response.create",`+policyTransportInput+`}`)
				} else {
					stream := transport == "sse"
					status, body := h.httpRequest(t, "/v1/responses", fmt.Sprintf(`{%s,"stream":%t}`, policyTransportInput, stream), nil)
					if status != http.StatusOK {
						t.Fatalf("status %d: %s", status, body)
					}
					output = body
					if stream && !strings.Contains(output, "data:") {
						t.Fatalf("missing SSE framing: %s", output)
					}
					if !stream && !json.Valid([]byte(output)) {
						t.Fatalf("nonstream response is not JSON: %s", output)
					}
				}
				if !strings.Contains(output, "resp_"+tc.want) {
					t.Fatalf("expected completed response from %s: %s", tc.want, output)
				}
				observations, upgrades := h.fixture.captured()
				if len(observations) != 1 {
					t.Fatalf("upstream requests = %d, want 1", len(observations))
				}
				got := observations[0]
				if got.account != tc.want {
					t.Fatalf("upstream account = %s, want %s", got.account, tc.want)
				}
				wantTransport := "http"
				if transport == "websocket" {
					wantTransport = "websocket"
					if upgrades != 1 {
						t.Fatalf("actual upstream upgrades = %d", upgrades)
					}
				} else if upgrades != 0 {
					t.Fatalf("unexpected upstream websocket upgrade")
				}
				if got.transport != wantTransport {
					t.Fatalf("actual transport = %s, want %s", got.transport, wantTransport)
				}
				if got.path != "/responses" {
					t.Fatalf("upstream path = %s", got.path)
				}
				if gjson.GetBytes(got.payload, "instructions").String() != "fixture final payload" {
					t.Fatalf("payload override lost: %s", got.payload)
				}
				if gjson.GetBytes(got.payload, "metadata.fixture_filter").Exists() {
					t.Fatalf("filtered payload field restored: %s", got.payload)
				}
			})
		}
	}
}

func TestAccountPolicyHTTPAffinityBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, mode, extra, want string
		child                   bool
	}{
		{"safe_completed_request_migrates", "deadline_at_boundary", "", "a", false},
		{"strict_retains_eligible_binding", "strict", "", "b", false},
		{"previous_response_id_stays_pinned", "deadline_at_boundary", `,"previous_response_id":"resp_b"`, "b", false},
		{"encrypted_reasoning_stays_pinned", "deadline_at_boundary", `,"reasoning":{"encrypted_content":"sanitized-fixture-state"}`, "b", false},
		{"child_session_keeps_parent", "deadline_at_boundary", "", "b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPolicyTransportHarness(t, nil)
			h.source.settings.Affinity = tc.mode
			headers := http.Header{"X-Session-Id": {"fixture-parent"}}
			status, first := h.httpRequest(t, "/v1/responses", `{`+policyTransportInput+`}`, headers)
			if status != http.StatusOK || !strings.Contains(first, "resp_b") {
				t.Fatalf("initial status=%d output=%s", status, first)
			}
			h.source.update(h.a, func(s *accountpolicy.Snapshot) { s.Buckets[1].ResetAt = h.now.Add(30 * time.Minute) })
			if tc.child {
				headers.Set("X-Parent-Session-Id", "fixture-parent")
				headers.Set("X-Session-Id", "fixture-child")
			}
			status, second := h.httpRequest(t, "/v1/responses", `{`+policyTransportInput+tc.extra+`}`, headers)
			if status != http.StatusOK || !strings.Contains(second, "resp_"+tc.want) {
				t.Fatalf("boundary status=%d expected=%s output=%s", status, tc.want, second)
			}
			observations, _ := h.fixture.captured()
			if len(observations) != 2 || observations[1].account != tc.want {
				t.Fatalf("boundary upstream observations=%+v", observations)
			}
		})
	}
}

func TestAccountPolicySSEDoesNotSpliceAfterPriorityChange(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	h := newPolicyTransportHarness(t, &policyTransportFixture{pause: release})
	req, err := http.NewRequest(http.MethodPost, h.url+"/v1/responses", strings.NewReader(`{`+policyTransportInput+`,"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	response, errDo := h.client.Do(req)
	if errDo != nil {
		t.Fatal(errDo)
	}
	defer func() { _ = response.Body.Close() }()
	reader := bufio.NewReader(response.Body)
	first, errRead := reader.ReadString('\n')
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !strings.Contains(first, "resp_b") {
		t.Fatalf("first stream event = %s", first)
	}
	h.source.update(h.b, func(s *accountpolicy.Snapshot) { s.Buckets[0].UsedPercent = 100 })
	releaseOnce.Do(func() { close(release) })
	rest, errRest := io.ReadAll(reader)
	if errRest != nil {
		t.Fatal(errRest)
	}
	if !strings.Contains(string(rest), "resp_b") || strings.Contains(string(rest), "resp_a") {
		t.Fatalf("stream changed accounts: %s", rest)
	}
	observations, _ := h.fixture.captured()
	if len(observations) != 1 || observations[0].account != "b" {
		t.Fatalf("stream executed on multiple accounts: %+v", observations)
	}
}

func TestAccountPolicyWebsocketReuseContinuationAndReconnect(t *testing.T) {
	h := newPolicyTransportHarness(t, nil)
	conn := h.dial(t, http.Header{"X-Session-Id": {"fixture-ws-session"}})
	first := policyTransportTurn(t, conn, `{"type":"response.create",`+policyTransportInput+`,"tools":[{"type":"function","name":"fixture_tool","parameters":{"type":"object","properties":{}}}]}`)
	if !strings.Contains(first, "resp_b") {
		t.Fatal(first)
	}
	if gjson.Get(first, "response.output.0.call_id").String() != "fixture-call" {
		t.Fatalf("fixture did not complete a real tool call: %s", first)
	}
	// Make A more urgent after completion; a live websocket and a response ID
	// are account-specific continuation state and must retain B.
	h.source.update(h.a, func(s *accountpolicy.Snapshot) { s.Buckets[1].ResetAt = h.now.Add(30 * time.Minute) })
	second := policyTransportTurn(t, conn, `{"type":"response.create","model":"gpt-5-codex","previous_response_id":"resp_b","input":[{"type":"function_call_output","call_id":"fixture-call","output":"sanitized result"}]}`)
	if !strings.Contains(second, "resp_b") {
		t.Fatalf("live unsafe continuation migrated: %s", second)
	}
	observations, upgrades := h.fixture.captured()
	if upgrades != 1 || len(observations) != 2 {
		t.Fatalf("upgrades=%d requests=%d; connection was not reused", upgrades, len(observations))
	}
	if observations[1].account != "b" || !strings.Contains(string(observations[1].payload), "fixture-call") {
		t.Fatalf("tool continuation lost account or tool output: %s", observations[1].payload)
	}
	if gjson.GetBytes(observations[1].payload, `input.#(type=="function_call_output")#`).Get("#").Int() != 1 {
		t.Fatalf("tool output was lost or replayed: %s", observations[1].payload)
	}
	if gjson.GetBytes(observations[1].payload, "instructions").String() != "fixture final payload" || gjson.GetBytes(observations[1].payload, "metadata.fixture_filter").Exists() {
		t.Fatalf("continuation lost final payload rules: %s", observations[1].payload)
	}
	_ = conn.Close()
	// A fresh independent connection has no provider continuation and can
	// select A. This is a real disconnect/reconnect, with another upstream
	// websocket handshake rather than a transport metadata assertion.
	third := policyTransportTurn(t, h.dial(t, http.Header{"X-Session-Id": {"fixture-ws-independent"}}), `{"type":"response.create",`+policyTransportInput+`}`)
	if !strings.Contains(third, "resp_a") {
		t.Fatalf("new websocket did not reevaluate priority: %s", third)
	}
	_, upgrades = h.fixture.captured()
	if upgrades != 2 {
		t.Fatalf("reconnect actual upstream upgrades=%d, want 2", upgrades)
	}
}

func TestAccountPolicyCompatibleWebsocketFallbackAndCompact(t *testing.T) {
	t.Run("native_upgrade_rejection_does_not_replay_over_http", func(t *testing.T) {
		h := newPolicyTransportHarness(t, &policyTransportFixture{rejectWS: true})
		h.source.settings.ForceAccount = h.b
		conn := h.dial(t, nil)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create",`+policyTransportInput+`}`)); err != nil {
			t.Fatal(err)
		}
		for {
			_, body, errRead := conn.ReadMessage()
			if errRead != nil {
				// Existing handlers may close before emitting an error frame.
				// Either failure form must prevent HTTP replay of native state.
				if !websocket.IsCloseError(errRead, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					t.Fatal(errRead)
				}
				break
			}
			kind := gjson.GetBytes(body, "type").String()
			if kind == "response.completed" {
				t.Fatalf("upgrade rejection unexpectedly completed: %s", body)
			}
			if kind == "error" || kind == "response.failed" {
				break
			}
		}
		observations, upgrades := h.fixture.captured()
		if len(observations) != 0 || upgrades != 0 {
			t.Fatalf("native upgrade refusal replayed business payload: upgrades=%d observations=%+v", upgrades, observations)
		}
	})
	t.Run("http_only_credential_uses_same_selected_account", func(t *testing.T) {
		h := newPolicyTransportHarness(t, nil)
		credential, ok := h.manager.GetByID(h.b)
		if !ok {
			t.Fatal("fixture credential disappeared")
		}
		credential.Attributes["websockets"] = "false"
		if _, err := h.manager.Update(context.Background(), credential); err != nil {
			t.Fatal(err)
		}
		output := policyTransportTurn(t, h.dial(t, nil), `{"type":"response.create",`+policyTransportInput+`}`)
		if !strings.Contains(output, "resp_b") {
			t.Fatal(output)
		}
		observations, upgrades := h.fixture.captured()
		if upgrades != 0 || len(observations) != 1 || observations[0].transport != "http" || observations[0].account != "b" {
			t.Fatalf("fallback changed account or failed to use HTTP: upgrades=%d observations=%+v", upgrades, observations)
		}
		if gjson.GetBytes(observations[0].payload, "instructions").String() != "fixture final payload" || gjson.GetBytes(observations[0].payload, "metadata.fixture_filter").Exists() {
			t.Fatalf("fallback lost final payload rules: %s", observations[0].payload)
		}
	})
	t.Run("compact_uses_http_and_policy", func(t *testing.T) {
		h := newPolicyTransportHarness(t, nil)
		status, output := h.httpRequest(t, "/v1/responses/compact", `{`+policyTransportInput+`}`, nil)
		if status != http.StatusOK || !strings.Contains(output, "compact_b") {
			t.Fatalf("compact status=%d output=%s", status, output)
		}
		observations, upgrades := h.fixture.captured()
		if upgrades != 0 || len(observations) != 1 || observations[0].path != "/responses/compact" || observations[0].account != "b" {
			t.Fatalf("compact transport selection: upgrades=%d observations=%+v", upgrades, observations)
		}
		if gjson.GetBytes(observations[0].payload, "instructions").String() != "fixture final payload" || gjson.GetBytes(observations[0].payload, "metadata.fixture_filter").Exists() {
			t.Fatalf("compact lost final payload rules: %s", observations[0].payload)
		}
	})
}

func TestAccountPolicyTransportRejectsUnauthorizedClients(t *testing.T) {
	h := newPolicyTransportHarness(t, nil)
	for _, key := range []string{"", "Bearer invalid-fixture-key"} {
		req, err := http.NewRequest(http.MethodPost, h.url+"/v1/responses", strings.NewReader(`{`+policyTransportInput+`}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", key)
		response, errDo := h.client.Do(req)
		if errDo != nil {
			t.Fatal(errDo)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("HTTP unauthorized status=%d", response.StatusCode)
		}
		conn, responseWS, errDial := h.dialer.Dial(strings.Replace(h.url, "http://", "ws://", 1)+"/v1/responses", http.Header{"Authorization": {key}})
		if conn != nil {
			_ = conn.Close()
		}
		if responseWS != nil {
			_ = responseWS.Body.Close()
		}
		if errDial == nil || responseWS == nil || responseWS.StatusCode != http.StatusUnauthorized {
			t.Fatalf("websocket unauthorized err=%v response=%v", errDial, responseWS)
		}
	}
	observations, upgrades := h.fixture.captured()
	if len(observations) != 0 || upgrades != 0 {
		t.Fatal("unauthorized request reached upstream")
	}
}

func TestAccountPolicyForcedAccountCannotBypassRealIneligibility(t *testing.T) {
	for _, transport := range []string{"http", "sse", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			h := newPolicyTransportHarness(t, nil)
			h.source.settings.ForceAccount = h.b
			h.source.update(h.b, func(s *accountpolicy.Snapshot) { s.Buckets[0].UsedPercent = 100 })
			if transport == "websocket" {
				conn := h.dial(t, nil)
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create",`+policyTransportInput+`}`)); err != nil {
					t.Fatal(err)
				}
				for {
					_, body, errRead := conn.ReadMessage()
					if errRead != nil {
						if !websocket.IsCloseError(errRead, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
							t.Fatal(errRead)
						}
						break
					}
					kind := gjson.GetBytes(body, "type").String()
					if kind == "response.completed" {
						t.Fatalf("force bypassed short exhaustion: %s", body)
					}
					if kind == "error" || kind == "response.failed" {
						break
					}
				}
			} else {
				status, body := h.httpRequest(t, "/v1/responses", fmt.Sprintf(`{%s,"stream":%t}`, policyTransportInput, transport == "sse"), nil)
				if status != http.StatusServiceUnavailable {
					t.Fatalf("forced unavailable status=%d output=%s", status, body)
				}
			}
			observations, upgrades := h.fixture.captured()
			if len(observations) != 0 || upgrades != 0 {
				t.Fatal("forced unavailable account reached inference upstream")
			}
		})
	}
}
