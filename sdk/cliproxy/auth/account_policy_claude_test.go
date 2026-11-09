package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const claudeTestBody = `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"sig"},{"type":"text","text":"hi"}]},{"role":"user","content":"continue"}]}`

type claudeFixture struct {
	s     *EarliestDeadlineSelector
	p     *policyTestSource
	auths map[string]*Auth
	list  []*Auth
	now   time.Time
}

// Accounts are named so that the fill-first fallback prefers "a", the account
// with the latest weekly reset; "x" resets first and "m" in between.
func newClaudeFixture(t *testing.T) *claudeFixture {
	t.Helper()
	f := &claudeFixture{now: time.Date(2026, 11, 9, 16, 0, 0, 0, time.UTC), auths: map[string]*Auth{}}
	settings := accountpolicy.DefaultSettings()
	settings.Enabled = true
	settings.Automation = "auto_expiring"
	f.p = &policyTestSource{settings: settings, snapshots: map[string]accountpolicy.Snapshot{}}
	f.s = NewEarliestDeadlineSelector(f.p, &FillFirstSelector{})
	f.s.now = func() time.Time { return f.now }
	f.s.SetIdleCheck(func(string) bool { return false })
	t.Cleanup(f.s.Stop)
	for _, id := range []string{"a", "m", "x"} {
		a := &Auth{ID: id, Provider: "claude"}
		f.auths[id] = a
		f.list = append(f.list, a)
	}
	return f
}

func (f *claudeFixture) observe(id string, resetIn time.Duration, used float64) {
	a := f.auths[id]
	a.Quota.ObservedAt = f.now
	a.Quota.Signals = map[string]string{
		claudeWeeklyResetSignal:       strconv.FormatInt(f.now.Add(resetIn).Unix(), 10),
		claudeWeeklyUtilizationSignal: strconv.FormatFloat(used, 'f', -1, 64),
	}
}

// observeAt keeps an account's absolute reset while updating its utilization.
func (f *claudeFixture) observeAt(id string, reset time.Time, used float64) {
	f.observe(id, reset.Sub(f.now), used)
}

func claudeOptions(session string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{session}}, OriginalRequest: []byte(claudeTestBody), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "client-1"}}
}

func (f *claudeFixture) pick(t *testing.T, model string, opts cliproxyexecutor.Options) string {
	t.Helper()
	got, err := f.s.Pick(context.Background(), "claude", model, opts, f.list)
	if err != nil || got == nil {
		t.Fatalf("pick: %v %v", got, err)
	}
	return got.ID
}

func (f *claudeFixture) lastReason() string {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if len(f.p.decisions) == 0 {
		return ""
	}
	return f.p.decisions[len(f.p.decisions)-1].Reason
}

// bindToLatest binds a session to "a" while the other accounts are unobserved,
// then makes every account observed with a 1-hour pace history for "x".
func (f *claudeFixture) bindToLatest(t *testing.T, model string, opts cliproxyexecutor.Options, xUsedLater float64) time.Time {
	t.Helper()
	f.observe("a", 100*time.Hour, 0.5)
	if got := f.pick(t, model, opts); got != "a" {
		t.Fatalf("initial binding = %q", got)
	}
	xReset := f.now.Add(40 * time.Hour)
	f.observeAt("x", xReset, 0.10)
	f.observe("m", 60*time.Hour, 0.05)
	// Record the first pace sample for x, then advance two hours.
	f.s.observeClaude(f.auths["x"])
	f.now = f.now.Add(2 * time.Hour)
	f.observeAt("x", xReset, xUsedLater)
	return xReset
}

func TestClaudeNewSessionsUseEarliestWeeklyReset(t *testing.T) {
	f := newClaudeFixture(t)
	f.observe("a", 100*time.Hour, 0.5)
	f.observe("m", 60*time.Hour, 0.05)
	f.observe("x", 40*time.Hour, 0.10)
	if got := f.pick(t, "claude-opus-5-5", claudeOptions("new")); got != "x" {
		t.Fatalf("got %q want x", got)
	}
	if reason := f.lastReason(); reason != "claude_weekly_reset" {
		t.Fatalf("reason = %q", reason)
	}
}

func TestClaudeIncompleteEvidenceUsesFallback(t *testing.T) {
	f := newClaudeFixture(t)
	f.observe("a", 100*time.Hour, 0.5)
	f.observe("x", 40*time.Hour, 0.10)
	if got := f.pick(t, "claude-opus-5-5", claudeOptions("one")); got != "a" {
		t.Fatalf("fallback picked %q want a", got)
	}
	if reason := f.lastReason(); reason != "missing_or_stale_evidence" {
		t.Fatalf("reason = %q", reason)
	}
	f.observe("m", 60*time.Hour, 0.05)
	if got := f.pick(t, "claude-opus-5-5", claudeOptions("two")); got != "x" {
		t.Fatalf("complete evidence picked %q want x", got)
	}
}

func TestClaudePassedResetRanksAsNextWeek(t *testing.T) {
	f := newClaudeFixture(t)
	f.observe("x", -time.Hour, 0.90)
	f.observe("m", 60*time.Hour, 0.05)
	f.observe("a", 100*time.Hour, 0.5)
	if got := f.pick(t, "claude-opus-5-5", claudeOptions("new")); got != "m" {
		t.Fatalf("got %q want m", got)
	}
}

func TestClaudeFocusMovesPortableConversationWhenAllowanceWouldExpire(t *testing.T) {
	f := newClaudeFixture(t)
	opts := claudeOptions("thread")
	f.bindToLatest(t, "claude-opus-5-5", opts, 0.11)
	if got := f.pick(t, "claude-opus-5-5", opts); got != "x" {
		t.Fatalf("got %q want x", got)
	}
	if reason := f.lastReason(); reason != "claude_focus_migration_from_a" {
		t.Fatalf("reason = %q", reason)
	}
	// The moved conversation stays on the focus account afterwards.
	if got := f.pick(t, "claude-opus-5-5", opts); got != "x" {
		t.Fatalf("after move got %q want x", got)
	}
}

func TestClaudeFocusKeepsConversationsThatCannotMove(t *testing.T) {
	for _, tc := range []struct {
		name, model, body string
		used              float64
		resetIn           time.Duration
		off               bool
	}{
		{name: "allowance_will_be_used", model: "claude-opus-5-5", used: 0.60},
		{name: "account_bound_thinking", model: "claude-sonnet-5-5", used: 0.11},
		{name: "haiku_thinking", model: "claude-haiku-5-5", used: 0.11},
		{name: "code_execution_container", model: "claude-opus-5-5", used: 0.11, body: `{"container":"container_1","messages":[{"role":"user","content":"hi"}]}`},
		{name: "files_api_document", model: "claude-opus-5-5", used: 0.11, body: `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file_1"}}]}]}`},
		{name: "server_web_search", model: "claude-opus-5-5", used: 0.11, body: `{"messages":[{"role":"assistant","content":[{"type":"web_search_tool_result","content":[]}]},{"role":"user","content":"go"}]}`},
		{name: "focus_off", model: "claude-opus-5-5", used: 0.11, off: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClaudeFixture(t)
			opts := claudeOptions("thread")
			f.bindToLatest(t, tc.model, opts, tc.used)
			if tc.off {
				f.p.settings.ClaudeFocus = "off"
			}
			if tc.body != "" {
				opts.OriginalRequest = []byte(tc.body)
			}
			if got := f.pick(t, tc.model, opts); got != "a" {
				t.Fatalf("got %q want a", got)
			}
		})
	}
}

func TestClaudeFocusNeedsResetWithinHorizonAndPaceHistory(t *testing.T) {
	f := newClaudeFixture(t)
	opts := claudeOptions("thread")
	f.observe("a", 100*time.Hour, 0.5)
	if got := f.pick(t, "claude-opus-5-5", opts); got != "a" {
		t.Fatalf("initial binding = %q", got)
	}
	f.observe("m", 70*time.Hour, 0.05)
	f.observe("x", 60*time.Hour, 0.10)
	f.s.observeClaude(f.auths["x"])
	f.now = f.now.Add(2 * time.Hour)
	if got := f.pick(t, "claude-opus-5-5", opts); got != "a" {
		t.Fatalf("reset beyond horizon moved conversation to %q", got)
	}
	// Fresh observations without an hour of history cannot justify a move.
	g := newClaudeFixture(t)
	g.observe("a", 100*time.Hour, 0.5)
	if got := g.pick(t, "claude-opus-5-5", opts); got != "a" {
		t.Fatalf("initial binding = %q", got)
	}
	g.observe("m", 60*time.Hour, 0.05)
	g.observe("x", 40*time.Hour, 0.10)
	if got := g.pick(t, "claude-opus-5-5", opts); got != "a" {
		t.Fatalf("missing pace history moved conversation to %q", got)
	}
}

func TestClaudeFocusStopsWhenNoLeftoverIsProjected(t *testing.T) {
	f := newClaudeFixture(t)
	opts := claudeOptions("thread")
	xReset := f.bindToLatest(t, "claude-opus-5-5", opts, 0.11)
	f.s.observeClaude(f.auths["x"])
	if target := f.s.claudeFocusTarget(f.now); target != "x" {
		t.Fatalf("focus target = %q", target)
	}
	// Eighty more points in two hours project the allowance fully used.
	f.now = f.now.Add(2 * time.Hour)
	f.observeAt("x", xReset, 0.91)
	f.s.observeClaude(f.auths["x"])
	if target := f.s.claudeFocusTarget(f.now); target != "" {
		t.Fatalf("focus stayed active on %q", target)
	}
}

func TestClaudeUnavailableOwnerMovesReplayableTurnWithoutIdleAccount(t *testing.T) {
	f := newClaudeFixture(t)
	opts := claudeOptions("thread")
	f.observe("a", 100*time.Hour, 0.5)
	if got := f.pick(t, "claude-opus-5-5", opts); got != "a" {
		t.Fatalf("initial binding = %q", got)
	}
	f.observe("m", 60*time.Hour, 0.05)
	f.observe("x", 40*time.Hour, 0.10)
	f.p.settings.Accounts = map[string]accountpolicy.AccountControl{"a": {Hold: true}}
	// The idle check reports active requests on "a"; Claude must not return 409.
	if got := f.pick(t, "claude-opus-5-5", opts); got != "x" {
		t.Fatalf("got %q want x", got)
	}
}

func TestClaudeFocusSettingValidation(t *testing.T) {
	settings := accountpolicy.DefaultSettings()
	for _, value := range []string{"", "auto", "off"} {
		settings.ClaudeFocus = value
		if err := accountpolicy.ValidateSettings(settings); err != nil {
			t.Fatalf("%q rejected: %v", value, err)
		}
	}
	settings.ClaudeFocus = "force"
	if err := accountpolicy.ValidateSettings(settings); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("force accepted: %v", err)
	}
}
