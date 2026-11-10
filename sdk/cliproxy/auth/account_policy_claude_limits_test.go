package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// The manager routes every request under the multi-provider namespace "mixed",
// so these tests use it and install the manager's auth resolver.
func newMixedClaudeFixture(t *testing.T) *claudeFixture {
	t.Helper()
	f := newClaudeFixture(t)
	f.s.SetBindingAuthResolver(func(id string) (*Auth, bool) {
		a, ok := f.auths[id]
		return a, ok
	})
	return f
}

func (f *claudeFixture) pickMixed(t *testing.T, model string, opts cliproxyexecutor.Options) string {
	t.Helper()
	got, err := f.s.Pick(context.Background(), "mixed", model, opts, f.list)
	if err != nil || got == nil {
		t.Fatalf("pick %s: %v %v", model, got, err)
	}
	return got.ID
}

// observeAll gives every account a weekly window: x resets first, then m, then a.
func (f *claudeFixture) observeAll() {
	f.observe("a", 100*time.Hour, 0.5)
	f.observe("m", 60*time.Hour, 0.05)
	f.observe("x", 40*time.Hour, 0.10)
}

// limit adds one window to the account's stored snapshot, as a response or the
// usage poller would, keeping the weekly window it already reports.
func (f *claudeFixture) limit(id, window string, used float64, resetIn time.Duration, status string) {
	a := f.auths[id]
	signals := make(map[string]string, len(a.Quota.Signals)+3)
	for k, v := range a.Quota.Signals {
		signals[k] = v
	}
	prefix := "Anthropic-Ratelimit-Unified-" + window + "-"
	signals[prefix+"Utilization"] = strconv.FormatFloat(used, 'f', -1, 64)
	signals[prefix+"Reset"] = strconv.FormatInt(f.now.Add(resetIn).Unix(), 10)
	if status != "" {
		signals[prefix+"Status"] = status
	}
	a.Quota.Signals = signals
	a.Quota.ObservedAt = f.now
}

func mixedOpts(session string) cliproxyexecutor.Options { return claudeOptions(session) }

func TestClaudeMixedProviderUnavailableOwnerMovesReplayableTurn(t *testing.T) {
	f := newMixedClaudeFixture(t)
	opts := mixedOpts("thread")
	f.observe("a", 100*time.Hour, 0.5)
	if got := f.pickMixed(t, "claude-opus-5-5", opts); got != "a" {
		t.Fatalf("initial binding = %q", got)
	}
	f.observe("m", 60*time.Hour, 0.05)
	f.observe("x", 40*time.Hour, 0.10)
	f.p.settings.Accounts = map[string]accountpolicy.AccountControl{"a": {Hold: true}}
	// "a" still serves other requests; the turn must move instead of failing with 409.
	if got := f.pickMixed(t, "claude-opus-5-5", opts); got != "x" {
		t.Fatalf("got %q want x", got)
	}
}

func TestMixedProviderUnavailableNonClaudeOwnerStaysPinned(t *testing.T) {
	f := newMixedClaudeFixture(t)
	f.auths["c"] = &Auth{ID: "c", Provider: "codex"}
	err := (&deadlineRankingSelector{owner: f.s}).PickUnavailable(context.Background(), "mixed", "gpt-6.1-sol", claudeOptions("thread"), "c")
	var rejection *Error
	if !errors.As(err, &rejection) || rejection.Code != "unsafe_affinity_unavailable" {
		t.Fatalf("busy Codex owner must stay pinned, got %v", err)
	}
	if err := (&deadlineRankingSelector{owner: f.s}).PickUnavailable(context.Background(), "mixed", "claude-opus-5-5", claudeOptions("thread"), "gone"); err == nil {
		t.Fatal("an unknown owner must not be treated as Claude")
	}
}

func TestClaudeFullFiveHourWindowSendsWorkToNextOpenAccount(t *testing.T) {
	f := newMixedClaudeFixture(t)
	f.observeAll()
	if got := f.pickMixed(t, "claude-opus-5-5", mixedOpts("before")); got != "x" {
		t.Fatalf("earliest weekly reset = %q", got)
	}
	f.limit("x", "5h", 1, time.Hour, "")
	for _, model := range []string{"claude-opus-5-5", "claude-fable-5-2", "claude-haiku-5-5"} {
		if got := f.pickMixed(t, model, mixedOpts("new-"+model)); got != "m" {
			t.Fatalf("%s with x's 5-hour window full = %q want m", model, got)
		}
	}
	// An almost full window is still open.
	f.limit("m", "5h", 0.99, time.Hour, "allowed_warning")
	if got := f.pickMixed(t, "claude-opus-5-5", mixedOpts("almost")); got != "m" {
		t.Fatalf("open 5-hour window skipped: %q", got)
	}
	f.now = f.now.Add(time.Hour + time.Second)
	if got := f.pickMixed(t, "claude-opus-5-5", mixedOpts("after-reset")); got != "x" {
		t.Fatalf("after the 5-hour reset = %q want x", got)
	}
}

func TestClaudeFullFableWindowMovesOnlyFable(t *testing.T) {
	f := newMixedClaudeFixture(t)
	f.observeAll()
	f.limit("x", "7d_oi", 1.02, 40*time.Hour, "rejected")
	if got := f.pickMixed(t, "claude-fable-5-2", mixedOpts("fable")); got != "m" {
		t.Fatalf("Fable with x's Fable window full = %q want m", got)
	}
	if got := f.pickMixed(t, "claude-opus-5-5", mixedOpts("opus")); got != "x" {
		t.Fatalf("Opus must stay on the earliest account, got %q", got)
	}
	// A later Opus response carries no Fable window; the full window persists.
	f.now = f.now.Add(time.Minute)
	f.observe("x", 40*time.Hour-time.Minute, 0.12)
	if got := f.pickMixed(t, "claude-fable-5-2", mixedOpts("fable-later")); got != "m" {
		t.Fatalf("Fable window forgotten after an Opus response: %q", got)
	}
}

func TestClaudeBoundFableConversationLeavesFullFableWindow(t *testing.T) {
	f := newMixedClaudeFixture(t)
	f.observeAll()
	fable, opus := mixedOpts("fable-thread"), mixedOpts("opus-thread")
	if got := f.pickMixed(t, "claude-fable-5-2", fable); got != "x" {
		t.Fatalf("initial Fable binding = %q", got)
	}
	if got := f.pickMixed(t, "claude-opus-5-5", opus); got != "x" {
		t.Fatalf("initial Opus binding = %q", got)
	}
	// The refusal itself reports the full Fable window; x stays busy with Opus.
	f.s.ObserveResponseHeaders("x", "claude", 0, http.Header{
		"Anthropic-Ratelimit-Unified-Status":       {"rejected"},
		"Anthropic-Ratelimit-Unified-7d-Status":    {"allowed"},
		"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":  {strconv.FormatInt(f.now.Add(40*time.Hour).Unix(), 10)},
	}, f.now)
	if got := f.pickMixed(t, "claude-fable-5-2", fable); got != "m" {
		t.Fatalf("bound Fable conversation = %q want m", got)
	}
	if got := f.pickMixed(t, "claude-opus-5-5", opus); got != "x" {
		t.Fatalf("bound Opus conversation = %q want x", got)
	}
}

func TestClaudeAllAccountsFullKeepsRouting(t *testing.T) {
	f := newMixedClaudeFixture(t)
	f.observeAll()
	for _, id := range []string{"a", "m", "x"} {
		f.limit(id, "5h", 1, time.Hour, "rejected")
	}
	if got := f.pickMixed(t, "claude-opus-5-5", mixedOpts("all-full")); got != "x" {
		t.Fatalf("all accounts full = %q want x (upstream decides)", got)
	}
}

func TestClaudeLimitObservationOrderingAndUnknownReset(t *testing.T) {
	f := newMixedClaudeFixture(t)
	f.observeAll()
	reset := strconv.FormatInt(f.now.Add(time.Hour).Unix(), 10)
	f.s.ObserveResponseHeaders("x", "mixed", 0, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"1"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}, f.now)
	f.s.ObserveResponseHeaders("x", "mixed", 0, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.2"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}, f.now.Add(-time.Second))
	if !f.s.claudeLimitBlocked("x", 0, "claude-opus-5-5", f.now) {
		t.Fatal("an older observation reopened a full window")
	}
	// A long stream completes late with headers from before the window filled.
	f.s.ObserveResponseHeaders("x", "claude", 0, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.2"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}, f.now.Add(time.Second))
	if !f.s.claudeLimitBlocked("x", 0, "claude-opus-5-5", f.now) {
		t.Fatal("a late open reading of the same window reopened it")
	}
	// The next window has a later reset and reopens the account.
	nextReset := strconv.FormatInt(f.now.Add(6*time.Hour).Unix(), 10)
	f.s.ObserveResponseHeaders("x", "claude", 0, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.01"}, "Anthropic-Ratelimit-Unified-5h-Reset": {nextReset}}, f.now.Add(2*time.Second))
	if f.s.claudeLimitBlocked("x", 0, "claude-opus-5-5", f.now) {
		t.Fatal("a newer window did not reopen the account")
	}
	// A late full reading of the previous window must not close the new one.
	f.s.ObserveResponseHeaders("x", "claude", 0, http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}, f.now.Add(3*time.Second))
	if f.s.claudeLimitBlocked("x", 0, "claude-opus-5-5", f.now) {
		t.Fatal("an observation of an earlier window replaced the current one")
	}
	f.s.ObserveResponseHeaders("m", "claude", 0, http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}}, f.now)
	if f.s.claudeLimitBlocked("m", 0, "claude-opus-5-5", f.now) {
		t.Fatal("a full window without a reset must be left to the reactive cooldown")
	}
	f.s.ObserveResponseHeaders("removed", "claude", 0, http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}, f.now)
	f.s.mu.Lock()
	_, recorded := f.s.claudeLimits["removed"]
	f.s.mu.Unlock()
	if recorded {
		t.Fatal("a result for a removed account recreated its limit state")
	}
	f.auths["c"] = &Auth{ID: "c", Provider: "codex"}
	f.s.ObserveResponseHeaders("c", "mixed", 0, http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}, f.now)
	if f.s.claudeLimitBlocked("c", 0, "claude-opus-5-5", f.now) {
		t.Fatal("a non-Claude account recorded a Claude window")
	}
}

type recordingHeaderSelector struct {
	RoundRobinSelector
	mu   sync.Mutex
	seen []http.Header
}

func (r *recordingHeaderSelector) ObserveResponseHeaders(authID, provider string, _ uint64, headers http.Header, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if authID == "claude-limit-hook" && provider == "claude" {
		r.seen = append(r.seen, headers)
	}
}

func TestMarkResultPassesRefusalHeadersToSelector(t *testing.T) {
	selector := &recordingHeaderSelector{}
	m := NewManager(nil, selector, nil)
	if _, err := m.Register(context.Background(), &Auth{ID: "claude-limit-hook", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	ctx := logging.WithResponseHeadersHolder(context.Background())
	logging.SetResponseHeaders(ctx, http.Header{"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"}})
	m.MarkResult(ctx, Result{AuthID: "claude-limit-hook", Provider: "claude", Model: "claude-fable-5-2", Error: &Error{HTTPStatus: http.StatusTooManyRequests}})
	selector.mu.Lock()
	defer selector.mu.Unlock()
	if len(selector.seen) != 1 || selector.seen[0].Get("Anthropic-Ratelimit-Unified-7d_oi-Status") != "rejected" {
		t.Fatalf("selector saw %v", selector.seen)
	}
}

func TestClaudeUnavailableOwnerKeepsAccountBoundTurn(t *testing.T) {
	for _, idle := range []bool{false, true} {
		f := newMixedClaudeFixture(t)
		f.s.SetIdleCheck(func(string) bool { return idle })
		opts := claudeOptions("files")
		opts.OriginalRequest = []byte(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file_1"}},{"type":"text","text":"summarize"}]}]}`)
		f.observe("a", 100*time.Hour, 0.5)
		if got := f.pickMixed(t, "claude-opus-5-5", opts); got != "a" {
			t.Fatalf("initial binding = %q", got)
		}
		f.observe("x", 40*time.Hour, 0.10)
		f.observe("m", 60*time.Hour, 0.05)
		f.limit("a", "5h", 1, time.Hour, "rejected")
		_, err := f.s.Pick(context.Background(), "mixed", "claude-opus-5-5", opts, f.list)
		var rejection *Error
		if !errors.As(err, &rejection) || rejection.Code != "unsafe_affinity_unavailable" {
			t.Fatalf("idle=%v: a turn referencing the owner's files moved: %v", idle, err)
		}
	}
}

// Sonnet and Haiku thinking is dropped, not refused, on another account, so a
// turn whose owner is blocked continues there instead of failing for hours.
func TestClaudeUnavailableOwnerMovesSonnetTurn(t *testing.T) {
	f := newMixedClaudeFixture(t)
	opts := mixedOpts("sonnet")
	f.observe("a", 100*time.Hour, 0.5)
	if got := f.pickMixed(t, "claude-sonnet-5-5", opts); got != "a" {
		t.Fatalf("initial binding = %q", got)
	}
	f.observe("x", 40*time.Hour, 0.10)
	f.observe("m", 60*time.Hour, 0.05)
	f.limit("a", "5h", 1, time.Hour, "rejected")
	if got := f.pickMixed(t, "claude-sonnet-5-5", opts); got != "x" {
		t.Fatalf("got %q want x", got)
	}
}

// Results reach the selector through the real manager in completion order.
func TestClaudeLateStreamResultKeepsFullWindowThroughManager(t *testing.T) {
	f := newClaudeFixture(t)
	f.s.now = time.Now
	m := NewManager(nil, f.s, nil)
	f.s.SetBindingAuthResolver(m.GetByID)
	if _, err := m.Register(context.Background(), &Auth{ID: "x", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	mark := func(headers http.Header, success bool) {
		ctx := logging.WithResponseHeadersHolder(context.Background())
		logging.SetResponseHeaders(ctx, headers)
		result := Result{AuthID: "x", Provider: "claude", Model: "claude-opus-5-5", Success: success}
		if !success {
			result.Error = &Error{HTTPStatus: http.StatusTooManyRequests}
		}
		m.MarkResult(ctx, result)
	}
	// A short request fills the window; a stream that started earlier finishes later.
	mark(http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Utilization": {"1.0"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}, false)
	mark(http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"allowed"}, "Anthropic-Ratelimit-Unified-5h-Utilization": {"0.9"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}, true)
	if !f.s.claudeLimitBlocked("x", 0, "claude-opus-5-5", time.Now()) {
		t.Fatal("a late stream reopened the full 5-hour window")
	}
}

func TestClaudeResetlessReadingKeepsKnownFullWindow(t *testing.T) {
	f := newMixedClaudeFixture(t)
	reset := strconv.FormatInt(f.now.Add(time.Hour).Unix(), 10)
	f.s.ObserveResponseHeaders("x", "claude", 0, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"1"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}, f.now)
	for i, headers := range []http.Header{
		{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.2"}},
		{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.2"}, "Anthropic-Ratelimit-Unified-5h-Reset": {"0"}},
		{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.2"}, "Anthropic-Ratelimit-Unified-5h-Reset": {"bad"}},
		{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}},
	} {
		f.s.ObserveResponseHeaders("x", "claude", 0, headers, f.now.Add(time.Duration(i+1)*time.Second))
		if !f.s.claudeLimitBlocked("x", 0, "claude-opus-5-5", f.now) {
			t.Fatalf("reading %d without a usable reset erased the full window", i)
		}
	}
	// Once the known reset passed, a reset-less reading is accepted.
	f.s.ObserveResponseHeaders("x", "claude", 0, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.1"}}, f.now.Add(2*time.Hour))
	f.s.mu.Lock()
	w := f.s.claudeLimits["x"].windows[claudeFiveHourWindow]
	f.s.mu.Unlock()
	if w.full {
		t.Fatal("a reading after the reset was ignored")
	}
}

func TestClaudeFreshContainerIsNotAccountBound(t *testing.T) {
	for body, bound := range map[string]bool{
		`{"container":null,"messages":[{"role":"user","content":"hi"}]}`:                                               false,
		`{"container":{"skills":[{"type":"anthropic","skill_id":"pdf"}]},"messages":[{"role":"user","content":"hi"}]}`: false,
		`{"container":"container_1","messages":[{"role":"user","content":"hi"}]}`:                                      true,
		`{"container":{"id":"container_1"},"messages":[{"role":"user","content":"hi"}]}`:                               true,
	} {
		if got := claudeRequestAccountBound([]byte(body)); got != bound {
			t.Fatalf("%s: bound=%v want %v", body, got, bound)
		}
	}
	for _, idle := range []bool{false, true} {
		f := newMixedClaudeFixture(t)
		f.s.SetIdleCheck(func(string) bool { return idle })
		opts := claudeOptions("fresh-container")
		opts.OriginalRequest = []byte(`{"container":null,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"again"}]}`)
		f.observe("a", 100*time.Hour, 0.5)
		if got := f.pickMixed(t, "claude-opus-5-5", opts); got != "a" {
			t.Fatalf("initial binding = %q", got)
		}
		f.observe("x", 40*time.Hour, 0.10)
		f.observe("m", 60*time.Hour, 0.05)
		f.limit("a", "5h", 1, time.Hour, "rejected")
		if got := f.pickMixed(t, "claude-opus-5-5", opts); got != "x" {
			t.Fatalf("idle=%v: fresh-container turn = %q want x", idle, got)
		}
	}
}

func TestClaudeLimitStateFollowsRegistrationEpoch(t *testing.T) {
	f := newMixedClaudeFixture(t)
	f.auths["x"].RegistrationEpoch = 2
	reset := strconv.FormatInt(f.now.Add(time.Hour).Unix(), 10)
	full := http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}
	f.s.ObserveResponseHeaders("x", "claude", 1, full, f.now)
	if f.s.claudeLimitBlocked("x", 2, "claude-opus-5-5", f.now) {
		t.Fatal("a result from an older registration was recorded")
	}
	f.s.ObserveResponseHeaders("x", "claude", 2, full, f.now)
	if !f.s.claudeLimitBlocked("x", 2, "claude-opus-5-5", f.now) {
		t.Fatal("current registration not recorded")
	}
	// The credential file is re-registered, possibly with another account.
	f.auths["x"].RegistrationEpoch = 3
	if f.s.claudeLimitBlocked("x", 3, "claude-opus-5-5", f.now) {
		t.Fatal("a new registration inherited the previous one's window")
	}
}

type pollRecordingSelector struct {
	RoundRobinSelector
	mu   sync.Mutex
	seen []uint64
}

func (r *pollRecordingSelector) ObserveResponseHeaders(_ string, _ string, epoch uint64, headers http.Header, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if headers.Get("Anthropic-Ratelimit-Unified-7d_oi-Utilization") == "1" {
		r.seen = append(r.seen, epoch)
	}
}

// A later Opus response replaces the auth's snapshot, so the poller hands its
// Fable window to the selector directly.
func TestClaudeUsagePollDeliversWindowsToSelector(t *testing.T) {
	selector := &pollRecordingSelector{}
	m := NewManager(nil, selector, nil)
	if _, err := m.Register(context.Background(), &Auth{ID: "claude-poll", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	a, _ := m.GetByID("claude-poll")
	headers := http.Header{"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"1"}, "Anthropic-Ratelimit-Unified-7d_oi-Reset": {strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}}
	if !m.observeClaudeUsage("claude-poll", a.RegistrationEpoch, headers, time.Now()) {
		t.Fatal("poll observation rejected")
	}
	selector.mu.Lock()
	defer selector.mu.Unlock()
	if len(selector.seen) != 1 || selector.seen[0] != a.RegistrationEpoch {
		t.Fatalf("selector saw %v", selector.seen)
	}
}
