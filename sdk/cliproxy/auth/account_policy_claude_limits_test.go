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
	f.s.ObserveResponseHeaders("x", "claude", http.Header{
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
	f.s.ObserveResponseHeaders("x", "mixed", http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"1"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}, f.now)
	f.s.ObserveResponseHeaders("x", "mixed", http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.2"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}, f.now.Add(-time.Second))
	if !f.s.claudeLimitBlocked("x", "claude-opus-5-5", f.now) {
		t.Fatal("an older observation reopened a full window")
	}
	f.s.ObserveResponseHeaders("x", "claude", http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.2"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}, f.now.Add(time.Second))
	if f.s.claudeLimitBlocked("x", "claude-opus-5-5", f.now) {
		t.Fatal("a newer open observation did not reopen the window")
	}
	f.s.ObserveResponseHeaders("m", "claude", http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}}, f.now)
	if f.s.claudeLimitBlocked("m", "claude-opus-5-5", f.now) {
		t.Fatal("a full window without a reset must be left to the reactive cooldown")
	}
	f.auths["c"] = &Auth{ID: "c", Provider: "codex"}
	f.s.ObserveResponseHeaders("c", "mixed", http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Reset": {reset}}, f.now)
	if f.s.claudeLimitBlocked("c", "claude-opus-5-5", f.now) {
		t.Fatal("a non-Claude account recorded a Claude window")
	}
}

type recordingHeaderSelector struct {
	RoundRobinSelector
	mu   sync.Mutex
	seen []http.Header
}

func (r *recordingHeaderSelector) ObserveResponseHeaders(authID, provider string, headers http.Header, _ time.Time) {
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
