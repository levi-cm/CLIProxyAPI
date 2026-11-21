package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudereset"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type fakeClaudeResetClient struct {
	mu          sync.Mutex
	statuses    map[string]claudereset.Observation
	identity    map[string][2]string
	claims      []string // "auth/grant/request"
	results     []string // consumed in order; "" means unknown outcome
	onClaim     func(authID, requestID string)
	statusReads map[string]int
	failStatus  map[string]bool
	// stale keeps an account's inventory unchanged after a reset, as when the
	// reset has not propagated yet.
	stale      map[string]bool
	staleFable map[string]bool
	onStatus   func(authID string)
	// headers makes Status return usage headers for the observation path.
	headers bool
}

func (f *fakeClaudeResetClient) Status(_ context.Context, a *Auth, _ string) (claudereset.Observation, http.Header, error) {
	if f.onStatus != nil {
		f.onStatus(a.ID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusReads[a.ID]++
	obs, ok := f.statuses[a.ID]
	if !ok || f.failStatus[a.ID] {
		return claudereset.Observation{}, nil, errors.New("no status")
	}
	if !f.headers {
		return obs, nil, nil
	}
	// Render usage as unified rate-limit headers, as the real client does.
	h := http.Header{}
	for _, item := range []struct {
		name string
		w    claudereset.Window
	}{{"7d", obs.Weekly}, {claudeFableWindow, obs.Fable}} {
		if item.w.Known {
			prefix := "Anthropic-Ratelimit-Unified-" + item.name + "-"
			h.Set(prefix+"Utilization", strconv.FormatFloat(item.w.Used/100, 'f', -1, 64))
			h.Set(prefix+"Reset", strconv.FormatInt(item.w.ResetAt.Unix(), 10))
		}
	}
	return obs, h, nil
}

func (f *fakeClaudeResetClient) Identity(_ context.Context, a *Auth, _ string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.identity[a.ID]
	if !ok {
		return "", "", errors.New("no identity")
	}
	return id[0], id[1], nil
}

func (f *fakeClaudeResetClient) Claim(_ context.Context, a *Auth, _ string, _ string, grantID, requestID string) (string, error) {
	if f.onClaim != nil {
		f.onClaim(a.ID, requestID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, a.ID+"/"+grantID+"/"+requestID)
	if len(f.results) == 0 {
		return "", errors.New("unexpected claim")
	}
	result := f.results[0]
	f.results = f.results[1:]
	if result == "" {
		return "", errors.New("timeout")
	}
	if result == "reset" {
		// As upstream: the grant is used and the cleared windows reopen; a
		// stale account still reports the old usage.
		obs := f.statuses[a.ID]
		if !f.stale[a.ID] {
			obs.Weekly.Used = 0
			if !f.staleFable[a.ID] {
				obs.Fable.Used = 0
			}
		}
		obs.Grants = append([]claudereset.Grant(nil), obs.Grants...)
		for i := range obs.Grants {
			if obs.Grants[i].ID == grantID {
				obs.Grants[i].ResetsLeft--
			}
		}
		f.statuses[a.ID] = obs
	}
	return result, nil
}

type claudeResetFixture struct {
	m      *Manager
	client *fakeClaudeResetClient
	c      *claudeResetController
	now    time.Time
	allow  bool
	dir    string
	locked bool
	onLock func()
	held   map[string]bool
}

func claudeResetObs(weeklyUsed float64, weeklyReset time.Time, grantLeft int) claudereset.Observation {
	return claudereset.Observation{
		Eligible: true,
		Weekly:   claudereset.Window{Known: true, Used: weeklyUsed, ResetAt: weeklyReset},
		Fable:    claudereset.Window{Known: true, Used: 0, ResetAt: weeklyReset},
		Grants: []claudereset.Grant{{
			ID: "full-reset-2611", Label: "One full limit reset", Class: claudereset.ClassWeekly, ClearsWeek: true, ClearsFable: true,
			ResetsTotal: 1, ResetsLeft: grantLeft, UsableNow: true,
			EndsAt: weeklyReset.Add(30 * 24 * time.Hour),
		}},
	}
}

func quotaCooldown(until time.Time) *ModelState {
	return &ModelState{Status: StatusError, Unavailable: true, NextRetryAfter: until, LastError: &Error{HTTPStatus: 429, Message: "limit"},
		Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: until}}
}

func newClaudeResetFixture(t *testing.T) *claudeResetFixture {
	t.Helper()
	// Manager cooldown bookkeeping uses the real clock, so the fixture clock
	// starts at real time and fixture deadlines stay in the future.
	f := &claudeResetFixture{m: NewManager(nil, nil, nil), now: time.Now().UTC().Truncate(time.Second), allow: true, dir: t.TempDir()}
	f.client = &fakeClaudeResetClient{statuses: map[string]claudereset.Observation{}, identity: map[string][2]string{}, statusReads: map[string]int{}, failStatus: map[string]bool{}, stale: map[string]bool{}, staleFable: map[string]bool{}}
	f.held = map[string]bool{}
	// Every account is out of weekly allowance; b renews last. Each account's
	// cooldowns end at its weekly reset (plus fuzz), as a weekly 429 sets them.
	resets := map[string]time.Time{"a": f.now.Add(48 * time.Hour), "b": f.now.Add(120 * time.Hour), "c": f.now.Add(72 * time.Hour)}
	for i, id := range []string{"a", "b", "c"} {
		account := "acct-" + id
		org := "11111111-1111-1111-1111-11111111111" + string(rune('1'+i))
		until := resets[id].Add(40 * time.Second)
		registerClaudeUsageAuth(t, f.m, id, func(a *Auth) {
			a.Metadata["account_uuid"] = account
			a.Metadata["organization_uuid"] = org
			a.ModelStates = map[string]*ModelState{"claude-opus-5-5": quotaCooldown(until), "claude-fable-5-2": quotaCooldown(until)}
		})
		f.client.identity[id] = [2]string{account, org}
	}
	f.client.statuses["a"] = claudeResetObs(100, resets["a"], 1)
	f.client.statuses["b"] = claudeResetObs(100, resets["b"], 1)
	f.client.statuses["c"] = claudeResetObs(99, resets["c"], 1)
	f.refused(time.Minute)
	f.c = newClaudeResetController(f.m, f.client, ClaudeResetOptions{
		Gate: func() (bool, string) { return f.allow, map[bool]string{true: "", false: "not_writer"}[f.allow] },
		Lock: func() (func(), error) {
			if f.locked {
				return nil, errors.New("held by the deployment runner")
			}
			if f.onLock != nil {
				f.onLock()
			}
			return func() {}, nil
		},
		StateDir: func() string { return f.dir },
		Held:     func(id string) bool { return f.held[id] },
		Now:      func() time.Time { return f.now },
	})
	f.m.claudeResets = f.c
	return f
}

// refused records a weekly-window refusal ago before the fixture clock.
func (f *claudeResetFixture) refused(ago time.Duration) {
	f.m.claudeResetMu.Lock()
	f.m.claudeWeeklyRefusalAt = f.now.Add(-ago)
	f.m.claudeResetMu.Unlock()
}

func (f *claudeResetFixture) ops(t *testing.T) []claudereset.Operation {
	t.Helper()
	j, err := claudereset.OpenJournal(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	return j.Operations()
}

func (f *claudeResetFixture) op(t *testing.T, credential string) claudereset.Operation {
	t.Helper()
	for _, op := range f.ops(t) {
		if op.CredentialID == credential {
			return op
		}
	}
	return claudereset.Operation{}
}

func TestClaudeResetClaimsJournaledResetAndRestoresRouting(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.client.onClaim = func(authID, requestID string) {
		ops := f.ops(t)
		if len(ops) != 1 || ops[0].State != claudereset.OpPending || ops[0].RequestID != requestID || ops[0].CredentialID != authID || ops[0].Attempts != 1 {
			t.Fatalf("claim sent before a pending journal entry: %+v", ops)
		}
	}
	f.client.results = []string{"reset"}
	stale := f.m.nextExecutionSeq() // a request dispatched before the reset
	f.c.tick(context.Background())

	if len(f.client.claims) != 1 || !strings.HasPrefix(f.client.claims[0], "b/full-reset-2611/") {
		t.Fatalf("claims = %v, want one claim on b", f.client.claims)
	}
	ops := f.ops(t)
	if len(ops) != 1 || ops[0].State != claudereset.OpDone || ops[0].Result != "reset" || ops[0].Trigger != claudereset.TriggerWeeklyNeed || ops[0].ResetsLeftBefore != 1 || !ops[0].Recovered || ops[0].AccountUUID != "acct-b" {
		t.Fatalf("ops = %+v", ops)
	}
	b, _ := f.m.GetByID("b")
	for model, st := range b.ModelStates {
		if st.Unavailable || st.Quota.Exceeded {
			t.Fatalf("%s cooldown not cleared on b: %+v", model, st)
		}
	}
	a, _ := f.m.GetByID("a")
	if st := a.ModelStates["claude-opus-5-5"]; st == nil || !st.Unavailable {
		t.Fatalf("cooldown changed on a: %+v", st)
	}
	// Pre-recovery dispatches can no longer re-apply b's cooldown or headers.
	f.m.mu.RLock()
	predates := f.m.resultPredatesQuotaRecoveryLocked("b", "claude-opus-5-5", stale, false)
	f.m.mu.RUnlock()
	if !predates {
		t.Fatal("no recovery watermark recorded for b")
	}

	// The settle period prevents a second need claim on the same evidence.
	f.client.results = []string{"reset"}
	f.now = f.now.Add(2 * time.Minute)
	f.c.tick(context.Background())
	if len(f.client.claims) != 1 {
		t.Fatalf("second claim during settle: %v", f.client.claims)
	}
}

func TestClaudeResetSuccessfulTrafficIsNotDemand(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.m.claudeResetMu.Lock()
	f.m.claudeWeeklyRefusalAt = time.Time{}
	f.m.claudeResetMu.Unlock()
	f.c.tick(context.Background())
	if len(f.client.claims) != 0 {
		t.Fatalf("claimed without a refused request: %v", f.client.claims)
	}
	status, _ := f.m.ClaudeResetStatus()
	if status.Weekly.State != claudereset.StateWaitingDemand {
		t.Fatalf("weekly = %+v", status.Weekly)
	}
}

func TestClaudeResetUnknownOutcomeRetriesWithSameRequestID(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.client.results = []string{"", "already_used"}
	f.c.tick(context.Background())
	if ops := f.ops(t); len(ops) != 1 || ops[0].State != claudereset.OpPending || ops[0].Attempts != 1 {
		t.Fatalf("ops after unknown outcome = %+v", ops)
	}
	// Within the retry spacing nothing is resent.
	f.now = f.now.Add(30 * time.Second)
	f.c.tick(context.Background())
	if len(f.client.claims) != 1 {
		t.Fatalf("claims = %v", f.client.claims)
	}
	f.now = f.now.Add(time.Minute)
	f.c.tick(context.Background())
	if len(f.client.claims) != 2 || f.client.claims[0] != f.client.claims[1] {
		t.Fatalf("retry did not reuse the request: %v", f.client.claims)
	}
	if ops := f.ops(t); len(ops) != 1 || ops[0].State != claudereset.OpDone || ops[0].Result != "already_used" || ops[0].Attempts != 2 {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestClaudeResetPendingResendNeedsCurrentDecision(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.client.results = []string{""}
	f.c.tick(context.Background())
	// Demand has passed: the unresolved claim is not resent.
	f.refused(time.Hour)
	f.now = f.now.Add(2 * time.Minute)
	f.c.tick(context.Background())
	if len(f.client.claims) != 1 {
		t.Fatalf("resent without a current need: %v", f.client.claims)
	}
	f.now = f.now.Add(claudereset.ClaimRetryWindow)
	f.c.tick(context.Background())
	if op := f.op(t, "b"); op.State != claudereset.OpFailed || op.Result != claudereset.ResultNotConsumed {
		t.Fatalf("op = %+v, want not_consumed", op)
	}
}

func TestClaudeResetPendingSettlesFromInventory(t *testing.T) {
	for _, tc := range []struct {
		left  int
		state string
	}{{0, claudereset.OpDone}, {1, claudereset.OpFailed}} {
		f := newClaudeResetFixture(t)
		f.client.results = []string{""}
		f.c.tick(context.Background())
		f.client.statuses["b"] = claudeResetObs(100, f.now.Add(120*time.Hour), tc.left)
		f.now = f.now.Add(claudereset.ClaimRetryWindow + time.Minute)
		f.refused(time.Minute)
		f.client.results = []string{"reset"}
		f.c.tick(context.Background())
		settledOp := f.op(t, "b")
		bClaims := 0
		for _, claim := range f.client.claims {
			if strings.HasPrefix(claim, "b/") {
				bClaims++
			}
		}
		if settledOp.State != tc.state || bClaims != 1 {
			t.Fatalf("left %d: b op = %+v claims = %v", tc.left, settledOp, f.client.claims)
		}
		// A spent reset restores the fleet; an unspent one lets the next account serve.
		if want := map[bool]int{true: 1, false: 2}[tc.left == 0]; len(f.client.claims) != want {
			t.Fatalf("left %d: claims = %v, want %d", tc.left, f.client.claims, want)
		}
	}
}

func TestClaudeResetRetryFindsSpentGrantWithoutResend(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.client.results = []string{""}
	f.c.tick(context.Background())
	// The first claim spent the grant; the next read shows it.
	f.client.statuses["b"] = claudeResetObs(0, f.now.Add(120*time.Hour), 0)
	f.now = f.now.Add(2 * time.Minute)
	f.c.tick(context.Background())
	if len(f.client.claims) != 1 {
		t.Fatalf("resent a spent claim: %v", f.client.claims)
	}
	if op := f.op(t, "b"); op.State != claudereset.OpDone || op.Result != claudereset.ResultReconciled || !op.Recovered {
		t.Fatalf("op = %+v", op)
	}
}

func TestClaudeResetGateBlocksClaimsButReportsInventory(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.allow = false
	f.c.tick(context.Background())
	if len(f.client.claims) != 0 {
		t.Fatalf("claimed without the writer gate: %v", f.client.claims)
	}
	status, ok := f.m.ClaudeResetStatus()
	if !ok || status.Active || status.InactiveReason != "not_writer" || len(status.Accounts) != 3 || len(status.Accounts["b"].Grants) != 1 {
		t.Fatalf("status = %+v", status)
	}
	if status.Planned == nil || status.Planned.AccountID != "b" {
		t.Fatalf("planned = %+v, want the claim a writer would make", status.Planned)
	}
}

func TestClaudeResetForeignTokenNeverReadsOrClaims(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.client.identity["b"] = [2]string{"someone-else", f.client.identity["b"][1]}
	f.client.results = []string{"reset"}
	f.c.tick(context.Background())
	if f.client.statusReads["b"] != 0 {
		t.Fatalf("inventory read with a foreign token: %v", f.client.statusReads)
	}
	// Without verified evidence b counts as open, so nothing is spent.
	if len(f.client.claims) != 0 {
		t.Fatalf("claims = %v", f.client.claims)
	}
	status, _ := f.m.ClaudeResetStatus()
	if status.Accounts["b"].Error == "" || len(status.Accounts["b"].Grants) != 0 {
		t.Fatalf("b status = %+v", status.Accounts["b"])
	}
}

func TestClaudeResetWaitsWhileAllowanceIsOpen(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.client.statuses["c"] = claudeResetObs(40, f.now.Add(72*time.Hour), 1)
	f.c.tick(context.Background())
	if len(f.client.claims) != 0 {
		t.Fatalf("claimed while allowance is open: %v", f.client.claims)
	}
	status, _ := f.m.ClaudeResetStatus()
	if status.Weekly.State != claudereset.StateOK || status.Weekly.Headroom != 60 {
		t.Fatalf("weekly = %+v", status.Weekly)
	}
}

type quotaRefusal struct{}

func (quotaRefusal) Error() string   { return "quota" }
func (quotaRefusal) StatusCode() int { return 429 }

func TestClaudeRefusalCountsOnlyWhenTheRequestFails(t *testing.T) {
	m := NewManager(nil, nil, nil)
	header := func(window string) http.Header {
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-"+window+"-Status", "rejected")
		return h
	}
	claude := []string{"claude"}
	request := func(window string) *policyRequestLease {
		opts, lease := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
		t.Cleanup(lease.release)
		resolved := "claude-opus-5-5"
		if window == claudeFableWindow {
			resolved = "claude-fable-5-2"
		}
		m.noteClaudeWindowRefusal(Result{Provider: "claude", Model: resolved, Options: opts}, header(window))
		return lease
	}
	// Another account served the retry: no need.
	m.noteClaudeRequestOutcome(claude, "claude-opus-5-5", nil, request("7d"))
	// A final 429 whose attempts hit only the 5-hour window, or a non-Claude route.
	m.noteClaudeRequestOutcome(claude, "claude-opus-5-5", quotaRefusal{}, request("5h"))
	m.noteClaudeRequestOutcome([]string{"openai-compatibility"}, "claude-opus-5-5", quotaRefusal{}, request("7d"))
	if r := m.claudeRefusalTimes(); !r.weekly.IsZero() || !r.localWeekly.IsZero() {
		t.Fatalf("refusal counted without a failed weekly request: %+v", r)
	}
	m.noteClaudeRequestOutcome(claude, "claude-opus-5-5", quotaRefusal{}, request("7d"))
	if r := m.claudeRefusalTimes(); r.weekly.IsZero() || !r.fable.IsZero() {
		t.Fatalf("weekly refusal = %+v", r)
	}
	// An alias route keeps the attribution of the resolved Fable model.
	m.noteClaudeRequestOutcome(claude, "fable", quotaRefusal{}, request("7d_oi"))
	if r := m.claudeRefusalTimes(); r.fable.IsZero() {
		t.Fatal("Fable refusal on an alias route not recorded")
	}
	// Without a controller to confirm a window cause, a local refusal is not need.
	m.noteClaudeRequestOutcome(claude, "claude-fable-5-2", newModelCooldownError("claude-fable-5-2", "claude", time.Hour), nil)
	if r := m.claudeRefusalTimes(); !r.localWeekly.IsZero() || !r.localFable.IsZero() {
		t.Fatalf("unconfirmed local refusal = %+v", r)
	}
}

func TestClaudeResetNoSecondSpendWhileResetNotVisible(t *testing.T) {
	for _, used := range []float64{100, 99} {
		f := newClaudeResetFixture(t)
		obs := f.client.statuses["b"]
		obs.Weekly.Used = used
		f.client.statuses["b"] = obs
		// b's reset is spent, but its inventory still shows the old usage, and
		// later reads fail.
		f.client.stale["b"] = true
		f.client.results = []string{"reset", "reset"}
		f.c.tick(context.Background())
		// A successful read after the claim still shows the old usage.
		f.now = f.now.Add(time.Minute)
		f.refused(time.Second)
		f.c.tick(context.Background())
		if len(f.client.claims) != 1 || f.op(t, "b").Recovered {
			t.Fatalf("used %v: claims = %v op = %+v", used, f.client.claims, f.op(t, "b"))
		}
		f.client.failStatus["b"] = true
		for i := 0; i < 15; i++ {
			f.now = f.now.Add(time.Minute)
			f.refused(time.Second)
			f.c.tick(context.Background())
		}
		if len(f.client.claims) != 1 {
			t.Fatalf("used %v: a second reset was spent before b's reset was visible: %v", used, f.client.claims)
		}
	}
}

func TestClaudeRefusalAttribution(t *testing.T) {
	m := NewManager(nil, nil, nil)
	claude := []string{"claude"}
	// An Opus request refused with 7d_oi rejected (an overage cap) is not Fable need.
	opts, lease := m.trackPolicyRequest(context.Background(), cliproxyexecutor.Options{})
	t.Cleanup(lease.release)
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "rejected")
	m.noteClaudeWindowRefusal(Result{Provider: "claude", Options: opts}, h)
	m.noteClaudeRequestOutcome(claude, "claude-opus-5-5", quotaRefusal{}, lease)
	if r := m.claudeRefusalTimes(); !r.fable.IsZero() {
		t.Fatalf("non-Fable request counted as Fable need: %+v", r)
	}
	// Selection refusing with auth_unavailable (cooling plus unusable
	// credentials) is a local refusal when the fleet is weekly-blocked now.
	f := newClaudeResetFixture(t)
	f.c.refresh(context.Background(), f.c.claudeAuths(), func(claudeResetStatusEntry) bool { return true })
	f.m.noteClaudeRequestOutcome(claude, "claude-opus-5-5", &Error{Code: "auth_unavailable", HTTPStatus: 503}, nil)
	if r := f.m.claudeRefusalTimes(); r.localWeekly.IsZero() {
		t.Fatal("auth_unavailable selection refusal not recorded")
	}
}

func TestRestoredNeedsADropFromTheBaseline(t *testing.T) {
	obs := claudereset.Observation{}
	w := func(used float64) claudereset.Window { return claudereset.Window{Known: true, Used: used} }
	b := func(used float64) *claudereset.WindowBaseline { return &claudereset.WindowBaseline{Used: used} }
	for _, tc := range []struct {
		before *claudereset.WindowBaseline
		after  claudereset.Window
		want   bool
	}{
		// Exhausted at claim time: only a clear drop proves the reset.
		{b(99), w(99), false},
		{b(99), w(95), false},
		{b(99), w(2), true},
		{b(100), w(100), false},
		{b(100), claudereset.Window{}, false},
		// Not exhausted at claim time: an open window is enough.
		{b(0), w(0), true},
		{b(4), w(3), true},
		{b(50), w(50), true},
		{nil, w(50), true},
	} {
		if got := restored(tc.before, obs, claudereset.WindowWeekly, tc.after); got != tc.want {
			t.Fatalf("restored(%+v, %+v) = %v, want %v", tc.before, tc.after, got, tc.want)
		}
	}
}

func TestClaudeResetRecoversEachWindowOnItsOwnEvidence(t *testing.T) {
	f := newClaudeResetFixture(t)
	obs := f.client.statuses["b"]
	obs.Fable.Used = 100
	f.client.statuses["b"] = obs
	// The weekly restore shows at once; Fable still shows the old usage.
	f.client.staleFable["b"] = true
	f.client.results = []string{"reset"}
	f.c.tick(context.Background())
	b, _ := f.m.GetByID("b")
	if b.ModelStates["claude-opus-5-5"].Unavailable || !b.ModelStates["claude-fable-5-2"].Unavailable {
		t.Fatalf("opus %+v fable %+v, want weekly cleared and Fable kept", b.ModelStates["claude-opus-5-5"], b.ModelStates["claude-fable-5-2"])
	}
	if f.op(t, "b").Recovered {
		t.Fatal("recovered before every cleared window was confirmed")
	}
	// A request dispatched after the reset keeps its evidence across later
	// recovery passes: the fence is taken once.
	afterReset := f.m.nextExecutionSeq()
	// Once Fable shows the reset, recovery completes.
	obs = f.client.statuses["b"]
	obs.Fable.Used = 0
	f.client.statuses["b"] = obs
	f.now = f.now.Add(time.Minute)
	f.c.tick(context.Background())
	f.m.mu.RLock()
	refenced := f.m.resultPredatesQuotaRecoveryLocked("b", "claude-opus-5-5", afterReset, false)
	f.m.mu.RUnlock()
	if refenced {
		t.Fatal("a later recovery pass fenced a request dispatched after the reset")
	}
	b, _ = f.m.GetByID("b")
	if b.ModelStates["claude-fable-5-2"].Unavailable || !f.op(t, "b").Recovered {
		t.Fatalf("fable %+v recovered %v", b.ModelStates["claude-fable-5-2"], f.op(t, "b").Recovered)
	}
}

func TestClaudeResetRecoveryFencesBeforeItsRead(t *testing.T) {
	f := newClaudeResetFixture(t)
	stale := f.m.nextExecutionSeq() // dispatched before the reset
	claimed := false
	fenced := false
	f.client.onClaim = func(string, string) { claimed = true }
	f.client.onStatus = func(id string) {
		if id == "b" && claimed {
			f.m.mu.RLock()
			fenced = f.m.resultPredatesQuotaRecoveryLocked("b", "claude-opus-5-5", stale, false)
			f.m.mu.RUnlock()
		}
	}
	f.client.results = []string{"reset"}
	f.c.tick(context.Background())
	if !claimed || !fenced {
		t.Fatalf("claimed=%v fenced during the recovery read=%v", claimed, fenced)
	}
}

func TestClaudeResetLocalRefusalNeedsWindowCause(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.m.claudeResetMu.Lock()
	f.m.claudeWeeklyRefusalAt = time.Time{}
	f.m.claudeResetMu.Unlock()
	f.c.refresh(context.Background(), f.c.claudeAuths(), func(claudeResetStatusEntry) bool { return true })
	refuse := func() {
		f.m.noteClaudeRequestOutcome([]string{"claude"}, "claude-opus-5-5", newModelCooldownError("claude-opus-5-5", "claude", time.Hour), nil)
	}
	// At refusal time c (99%) is cooling for another cause (a 5-hour window):
	// the refusal is not weekly need, and later fleet changes cannot make it one.
	c, _ := f.m.GetByID("c")
	c.ModelStates = map[string]*ModelState{"claude-opus-5-5": quotaCooldown(f.now.Add(3 * time.Hour))}
	if _, err := f.m.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	refuse()
	c, _ = f.m.GetByID("c")
	c.ModelStates = map[string]*ModelState{"claude-opus-5-5": quotaCooldown(f.client.statuses["c"].Weekly.ResetAt.Add(40 * time.Second))}
	if _, err := f.m.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	f.client.results = []string{"reset"}
	f.c.tick(context.Background())
	if len(f.client.claims) != 0 {
		t.Fatalf("claimed on a refusal whose cause was not weekly when it happened: %v", f.client.claims)
	}
	// A refusal while every account is weekly-blocked is need.
	refuse()
	f.now = f.now.Add(time.Minute)
	f.c.tick(context.Background())
	if len(f.client.claims) != 1 || !strings.HasPrefix(f.client.claims[0], "b/") {
		t.Fatalf("claims = %v, want b", f.client.claims)
	}
}

func TestClaudeResetLocalRefusalIgnoresRenewedOrStaleExhaustion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(f *claudeResetFixture)
	}{
		// a's cached reading says exhausted, but its window renewed since then.
		{"renewed", func(f *claudeResetFixture) {
			e := f.c.statuses["a"]
			e.obs.Weekly.ResetAt = f.now.Add(-time.Minute)
			f.c.statuses["a"] = e
		}},
		// a's exhausted reading is older than the freshness limit.
		{"stale", func(f *claudeResetFixture) {
			e := f.c.statuses["a"]
			e.obs.Weekly.ObservedAt = f.now.Add(-time.Hour)
			f.c.statuses["a"] = e
		}},
	} {
		f := newClaudeResetFixture(t)
		f.c.refresh(context.Background(), f.c.claudeAuths(), func(claudeResetStatusEntry) bool { return true })
		// No weekly cooldown backs a's state; only the reading could.
		a, _ := f.m.GetByID("a")
		a.ModelStates = map[string]*ModelState{"claude-opus-5-5": quotaCooldown(f.now.Add(3 * time.Hour))}
		if _, err := f.m.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		tc.mutate(f)
		f.m.noteClaudeRequestOutcome([]string{"claude"}, "claude-opus-5-5", newModelCooldownError("claude-opus-5-5", "claude", time.Hour), nil)
		if r := f.m.claudeRefusalTimes(); !r.localWeekly.IsZero() {
			t.Fatalf("%s: local refusal given a weekly cause: %+v", tc.name, r)
		}
	}
}

func TestClaudeResetNoSecondSpendWhileResetUnconfirmed(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.client.results = []string{"reset", "reset"}
	f.client.onClaim = func(authID, _ string) {
		if authID == "b" {
			// Inventory reads of b fail from the claim on.
			f.client.failStatus["b"] = true
		}
	}
	f.c.tick(context.Background())
	for i := 0; i < 15; i++ {
		f.now = f.now.Add(time.Minute)
		f.refused(time.Second)
		f.c.tick(context.Background())
	}
	if len(f.client.claims) != 1 {
		t.Fatalf("a second reset was spent while b's reset is unconfirmed: %v", f.client.claims)
	}
}

func TestClaudeResetRecoveryWaitsForVisibleReset(t *testing.T) {
	f := newClaudeResetFixture(t)
	j, err := claudereset.OpenJournal(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	reset := f.client.statuses["a"].Weekly.ResetAt
	done := claudereset.Operation{ID: "op", CredentialID: "a", AccountUUID: "acct-a", OrganizationUUID: f.client.identity["a"][1], GrantID: "full-reset-2611",
		Class: claudereset.ClassWeekly, ClearsWeekly: true, ClearsFable: true, Trigger: claudereset.TriggerWeeklyNeed, State: claudereset.OpDone, Result: "reset",
		WeeklyBefore: &claudereset.WindowBaseline{Used: 100, ResetAt: reset}, FableBefore: &claudereset.WindowBaseline{Used: 0, ResetAt: reset},
		CreatedAt: f.now.Add(-5 * time.Minute), UpdatedAt: f.now.Add(-5 * time.Minute), ConfirmedAt: f.now.Add(-5 * time.Minute)}
	if errSave := j.Save(done); errSave != nil {
		t.Fatal(errSave)
	}
	// Inventory still shows the week used up: nothing is cleared yet.
	f.c.tick(context.Background())
	if a, _ := f.m.GetByID("a"); !a.ModelStates["claude-opus-5-5"].Unavailable || f.op(t, "a").Recovered {
		t.Fatal("recovered before the reset was visible")
	}
	// After a restart restored the credential-wide cooldown and a passive read
	// advanced the credential's timestamp, the visible reset still clears it.
	a, _ := f.m.GetByID("a")
	a.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: reset.Add(40 * time.Second)}
	a.Unavailable, a.NextRetryAfter, a.LastError = true, reset.Add(40*time.Second), &Error{HTTPStatus: 429}
	a.UpdatedAt = f.now
	if _, err = f.m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	obs := f.client.statuses["a"]
	obs.Weekly.Used = 0
	f.client.statuses["a"] = obs
	f.now = f.now.Add(time.Minute)
	f.c.tick(context.Background())
	a, _ = f.m.GetByID("a")
	if a.Unavailable || a.Quota.Exceeded || a.ModelStates["claude-opus-5-5"].Unavailable || !f.op(t, "a").Recovered {
		t.Fatalf("credential = unavailable %v quota %+v opus %+v recovered %v", a.Unavailable, a.Quota, a.ModelStates["claude-opus-5-5"], f.op(t, "a").Recovered)
	}
}

func TestClaudeResetRespectsDeploymentFenceAndRecheckedGate(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.client.results = []string{"reset"}
	f.locked = true
	f.c.tick(context.Background())
	if len(f.client.claims) != 0 || len(f.ops(t)) != 0 {
		t.Fatalf("claimed or journaled while the fence was held: %v", f.client.claims)
	}
	// Losing writer authority while waiting for the fence also stops the claim.
	f.locked = false
	f.onLock = func() { f.allow = false }
	f.c.tick(context.Background())
	if len(f.client.claims) != 0 || len(f.ops(t)) != 0 {
		t.Fatalf("claimed after the gate closed: %v", f.client.claims)
	}
}

func TestClaudeResetFailedPreClaimReadStopsClaim(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.client.results = []string{"reset"}
	f.c.refresh(context.Background(), f.c.claudeAuths(), func(claudeResetStatusEntry) bool { return true })
	f.client.failStatus["b"] = true
	f.c.tick(context.Background())
	if len(f.client.claims) != 0 {
		t.Fatalf("claimed on a failed verification read: %v", f.client.claims)
	}
}

func TestClaudeResetReplacedCredentialIsNotClaimed(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.client.results = []string{""}
	f.c.tick(context.Background())
	if ops := f.ops(t); len(ops) != 1 || ops[0].State != claudereset.OpPending {
		t.Fatalf("ops = %+v", ops)
	}
	// The credential file now holds another account: no resend, no settlement
	// from the replacement's inventory.
	b, _ := f.m.GetByID("b")
	b.Metadata["account_uuid"] = "acct-other"
	if _, err := f.m.Update(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	f.client.identity["b"] = [2]string{"acct-other", f.client.identity["b"][1]}
	f.client.statuses["b"] = claudeResetObs(100, f.now.Add(120*time.Hour), 0)
	for _, step := range []time.Duration{2 * time.Minute, claudereset.ClaimRetryWindow, 30 * time.Minute} {
		f.now = f.now.Add(step)
		f.c.tick(context.Background())
	}
	if len(f.client.claims) != 1 {
		t.Fatalf("resent for a replaced credential: %v", f.client.claims)
	}
	if op := f.op(t, "b"); op.State != claudereset.OpPending {
		t.Fatalf("settled from another account's inventory: %+v", op)
	}
	f.now = f.now.Add(claudereset.UnresolvedAfter)
	f.c.tick(context.Background())
	if op := f.op(t, "b"); op.State != claudereset.OpFailed || op.Result != claudereset.ResultIdentityChanged {
		t.Fatalf("op = %+v, want identity_changed", op)
	}
}

func TestClaudeResetRestartAfterSendReconciles(t *testing.T) {
	f := newClaudeResetFixture(t)
	j, err := claudereset.OpenJournal(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	// A crash after the POST left the attempt journaled but not its outcome.
	sent := claudereset.Operation{ID: "op", RequestID: "req-1", CredentialID: "b", AccountUUID: "acct-b", OrganizationUUID: f.client.identity["b"][1],
		GrantID: "full-reset-2611", Class: claudereset.ClassWeekly, ClearsWeekly: true, ClearsFable: true, Trigger: claudereset.TriggerWeeklyNeed,
		ResetsLeftBefore: 1, State: claudereset.OpPending, Attempts: 1, CreatedAt: f.now.Add(-3 * time.Minute), UpdatedAt: f.now.Add(-3 * time.Minute)}
	if errSave := j.Save(sent); errSave != nil {
		t.Fatal(errSave)
	}
	f.client.statuses["b"] = claudeResetObs(0, f.now.Add(120*time.Hour), 0)
	f.c.tick(context.Background())
	if len(f.client.claims) != 0 {
		t.Fatalf("claims = %v, want the spent claim settled without a resend", f.client.claims)
	}
	if op := f.op(t, "b"); op.State != claudereset.OpDone || op.Result != claudereset.ResultReconciled || !op.Recovered {
		t.Fatalf("op = %+v", op)
	}
	// A resend refused while the inventory still shows the old count may
	// follow an earlier attempt that landed: the claim stays pending and
	// quarantined, and no other account is claimed for the same shortage.
	g := newClaudeResetFixture(t)
	j, err = claudereset.OpenJournal(g.dir)
	if err != nil {
		t.Fatal(err)
	}
	sent.CreatedAt, sent.UpdatedAt = g.now.Add(-3*time.Minute), g.now.Add(-3*time.Minute)
	if errSave := j.Save(sent); errSave != nil {
		t.Fatal(errSave)
	}
	g.client.results = []string{"rate_limited", "reset"}
	g.c.tick(context.Background())
	if op := g.op(t, "b"); len(g.client.claims) != 1 || g.client.claims[0] != "b/full-reset-2611/req-1" || op.Attempts != 2 || op.State != claudereset.OpPending {
		t.Fatalf("claims = %v op = %+v", g.client.claims, op)
	}
	// The post-window reconciliation decides: an unchanged count settles it
	// unspent, and only then may the next account serve the need.
	g.now = g.now.Add(claudereset.ClaimRetryWindow)
	g.refused(time.Second)
	g.c.tick(context.Background())
	if op := g.op(t, "b"); op.State != claudereset.OpFailed || op.Result != claudereset.ResultNotConsumed {
		t.Fatalf("op = %+v", op)
	}
}

func TestClaudeResetLocalRecoveryResumesAfterRestart(t *testing.T) {
	f := newClaudeResetFixture(t)
	j, err := claudereset.OpenJournal(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	// The reset was spent and journaled, but the process stopped before the
	// local recovery. Only Fable was restored.
	done := claudereset.Operation{ID: "op", CredentialID: "a", AccountUUID: "acct-a", OrganizationUUID: f.client.identity["a"][1], GrantID: "fable",
		Class: claudereset.ClassFable, ClearsFable: true, Trigger: claudereset.TriggerFableNeed, State: claudereset.OpDone, Result: "reset",
		FableBefore: &claudereset.WindowBaseline{Used: 100, ResetAt: f.client.statuses["a"].Fable.ResetAt},
		CreatedAt:   f.now.Add(-5 * time.Minute), UpdatedAt: f.now.Add(-5 * time.Minute), ConfirmedAt: f.now.Add(-5 * time.Minute)}
	if errSave := j.Save(done); errSave != nil {
		t.Fatal(errSave)
	}
	f.client.statuses["c"] = claudeResetObs(10, f.now.Add(72*time.Hour), 1)
	obs := f.client.statuses["a"]
	obs.Weekly.Used = 50
	f.client.statuses["a"] = obs
	f.c.tick(context.Background())
	a, _ := f.m.GetByID("a")
	if st := a.ModelStates["claude-fable-5-2"]; st == nil || st.Unavailable || st.Quota.Exceeded {
		t.Fatalf("Fable cooldown not cleared: %+v", st)
	}
	if st := a.ModelStates["claude-opus-5-5"]; st == nil || !st.Unavailable {
		t.Fatalf("a Fable reset cleared the Opus cooldown: %+v", st)
	}
	if ops := f.ops(t); len(ops) != 1 || !ops[0].Recovered {
		t.Fatalf("recovery not journaled: %+v", ops)
	}
}

func TestClaudeResetOneRefusalSpendsOneGrant(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.client.results = []string{"reset", "reset"}
	f.c.tick(context.Background())
	if len(f.client.claims) != 1 || !strings.HasPrefix(f.client.claims[0], "b/") || !f.op(t, "b").Recovered {
		t.Fatalf("claims = %v op = %+v", f.client.claims, f.op(t, "b"))
	}
	// b recovered and serves; eleven minutes later it is put on hold. The fleet
	// looks short again, but no request was refused since b's reset.
	f.held["b"] = true
	for i := 0; i < 4; i++ {
		f.now = f.now.Add(3 * time.Minute)
		f.c.tick(context.Background())
	}
	if len(f.client.claims) != 1 {
		t.Fatalf("one refusal spent a second grant: %v", f.client.claims)
	}
	// A new refusal is new need.
	f.refused(time.Second)
	f.c.tick(context.Background())
	if len(f.client.claims) != 2 || !strings.HasPrefix(f.client.claims[1], "c/") {
		t.Fatalf("claims = %v, want c after a new refusal", f.client.claims)
	}
}

func TestClaudeResetSkipsHeldAccount(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.held["b"] = true
	f.client.results = []string{"reset"}
	f.c.tick(context.Background())
	if len(f.client.claims) != 1 || !strings.HasPrefix(f.client.claims[0], "c/") {
		t.Fatalf("claims = %v, want c while b is on hold", f.client.claims)
	}
}

type failingCooldownStore struct{}

func (failingCooldownStore) Load(context.Context) ([]CooldownStateRecord, error) { return nil, nil }
func (failingCooldownStore) Save(context.Context, []CooldownStateRecord) error {
	return errors.New("disk full")
}

func TestRecoverClaudeResetGuards(t *testing.T) {
	f := newClaudeResetFixture(t)
	reset := f.client.statuses["a"].Weekly.ResetAt
	recovery := ClaudeResetRecovery{CredentialID: "a", AccountUUID: "acct-a", OrganizationUUID: f.client.identity["a"][1], ClearsWeekly: true, ClearsFable: true,
		WeeklyResetAt: reset, FableResetAt: reset, WeeklyOpen: true, FableOpen: true, ObservedAt: f.now.Add(-time.Minute), ConfirmedAt: f.now.Add(-2 * time.Minute)}
	wrong := recovery
	wrong.AccountUUID = "acct-other"
	if err := f.m.RecoverClaudeReset(context.Background(), wrong); err == nil {
		t.Fatal("recovered a credential that now holds another account")
	}
	f.m.SetCooldownStateStore(failingCooldownStore{})
	if err := f.m.RecoverClaudeReset(context.Background(), recovery); err == nil {
		t.Fatal("recovery reported success without a durable cooldown save")
	}
	if a, _ := f.m.GetByID("a"); !a.ModelStates["claude-opus-5-5"].Unavailable {
		t.Fatal("unpersisted recovery was published")
	}
	f.m.SetCooldownStateStore(nil)
	// A cooldown recorded after the post-reset read is a newer failure, and one
	// ending at another deadline (a 5-hour window) has another cause: both stay.
	a, _ := f.m.GetByID("a")
	a.ModelStates["claude-opus-5-5"].UpdatedAt = f.now
	a.ModelStates["claude-sonnet-5-5"] = quotaCooldown(f.now.Add(3 * time.Hour))
	if _, err := f.m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := f.m.RecoverClaudeReset(context.Background(), recovery); err != nil {
		t.Fatal(err)
	}
	a, _ = f.m.GetByID("a")
	if !a.ModelStates["claude-opus-5-5"].Unavailable || a.ModelStates["claude-fable-5-2"].Unavailable || !a.ModelStates["claude-sonnet-5-5"].Unavailable {
		t.Fatalf("states = opus %+v fable %+v sonnet %+v", a.ModelStates["claude-opus-5-5"], a.ModelStates["claude-fable-5-2"], a.ModelStates["claude-sonnet-5-5"])
	}
}

func TestRecoverClaudeResetKeepsCooldownsOfClosedWindows(t *testing.T) {
	f := newClaudeResetFixture(t)
	reset := f.client.statuses["a"].Weekly.ResetAt
	// A weekly-only grant: the post-reset read still shows Fable used up.
	r := ClaudeResetRecovery{CredentialID: "a", AccountUUID: "acct-a", OrganizationUUID: f.client.identity["a"][1], ClearsWeekly: true,
		WeeklyResetAt: reset, WeeklyOpen: true, FableOpen: false, ObservedAt: f.now, ConfirmedAt: f.now}
	if err := f.m.RecoverClaudeReset(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	a, _ := f.m.GetByID("a")
	if a.ModelStates["claude-opus-5-5"].Unavailable || !a.ModelStates["claude-fable-5-2"].Unavailable {
		t.Fatalf("weekly-only reset: opus %+v fable %+v", a.ModelStates["claude-opus-5-5"], a.ModelStates["claude-fable-5-2"])
	}
	// A deadline that also matches a still-full 5-hour window stays.
	g := newClaudeResetFixture(t)
	r.FableOpen, r.SessionFull, r.SessionResetAt = true, true, reset.Add(-time.Minute)
	if err := g.m.RecoverClaudeReset(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if a, _ = g.m.GetByID("a"); !a.ModelStates["claude-opus-5-5"].Unavailable {
		t.Fatal("cleared a cooldown that may belong to the full 5-hour window")
	}
}

type forgetWindowsSelector struct {
	FillFirstSelector
	windows []string
}

func (s *forgetWindowsSelector) ForgetClaudeLimits(_ string, _ uint64, windows []string, _ time.Time) {
	s.windows = append(s.windows, windows...)
}

func TestClaudeResetLaggingFullReadDoesNotStrandFable(t *testing.T) {
	f := newClaudeResetFixture(t)
	settings := accountpolicy.DefaultSettings()
	settings.Enabled = true
	sel := NewEarliestDeadlineSelector(&policyTestSource{settings: settings, snapshots: map[string]accountpolicy.Snapshot{}}, &FillFirstSelector{})
	sel.now = func() time.Time { return f.now }
	sel.SetBindingAuthResolver(f.m.GetByID)
	t.Cleanup(sel.Stop)
	f.m.SetSelector(sel)
	f.client.headers = true
	obs := f.client.statuses["b"]
	obs.Fable.Used = 100
	f.client.statuses["b"] = obs
	// The first post-claim read lags: Fable still shows full with the same reset.
	f.client.staleFable["b"] = true
	f.client.results = []string{"reset"}
	f.c.tick(context.Background())
	// A later read, after the claim was confirmed, still lags.
	f.now = f.now.Add(time.Minute)
	f.c.tick(context.Background())
	b, _ := f.m.GetByID("b")
	if !sel.claudeLimitBlocked("b", b.RegistrationEpoch, "claude-fable-5-2", f.now) {
		t.Fatal("setup: the lagging read did not mark Fable full")
	}
	// The reset shows up: the same deadline, now open.
	obs = f.client.statuses["b"]
	obs.Fable.Used = 0
	f.client.statuses["b"] = obs
	f.now = f.now.Add(time.Minute)
	f.c.tick(context.Background())
	if !f.op(t, "b").Recovered {
		t.Fatalf("op = %+v", f.op(t, "b"))
	}
	if sel.claudeLimitBlocked("b", b.RegistrationEpoch, "claude-fable-5-2", f.now) {
		t.Fatal("Fable stays blocked on the restored account after the reset became visible")
	}
}

func TestClaudeResetForcedAccountOnlyServes(t *testing.T) {
	f := newClaudeResetFixture(t)
	f.c.opts.Forced = func() string { return "a" }
	f.client.results = []string{"reset"}
	f.c.tick(context.Background())
	// Only a serves while forced; b and c renew later but cannot serve.
	if len(f.client.claims) != 1 || !strings.HasPrefix(f.client.claims[0], "a/") {
		t.Fatalf("claims = %v, want the forced account", f.client.claims)
	}
}

func TestRecoverClaudeResetFablePassChecksBothDeadlines(t *testing.T) {
	f := newClaudeResetFixture(t)
	weeklyReset := f.client.statuses["a"].Weekly.ResetAt
	fableReset := weeklyReset.Add(-24 * time.Hour)
	// The Fable cooldown came from the weekly window; the weekly pass ran
	// while Fable was still unconfirmed, so the Fable pass must check both.
	r := ClaudeResetRecovery{CredentialID: "a", AccountUUID: "acct-a", OrganizationUUID: f.client.identity["a"][1], ClearsWeekly: true, ClearsFable: true,
		WeeklyResetAt: weeklyReset, FableResetAt: fableReset, WeeklyOpen: true, FableOpen: false, ObservedAt: f.now, ConfirmedAt: f.now,
		Pass: &ClaudeResetPass{Weekly: true}, Fenced: true}
	if err := f.m.RecoverClaudeReset(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if a, _ := f.m.GetByID("a"); !a.ModelStates["claude-fable-5-2"].Unavailable {
		t.Fatal("Fable cooldown cleared before Fable was confirmed")
	}
	r.FableOpen, r.Pass = true, &ClaudeResetPass{Fable: true}
	if err := f.m.RecoverClaudeReset(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if a, _ := f.m.GetByID("a"); a.ModelStates["claude-fable-5-2"].Unavailable {
		t.Fatal("weekly-caused Fable cooldown stranded after the Fable pass")
	}
}

func TestRecoverClaudeResetFableRepairsCredentialWideCooldown(t *testing.T) {
	f := newClaudeResetFixture(t)
	weeklyReset := f.client.statuses["a"].Weekly.ResetAt
	fableReset := weeklyReset.Add(-24 * time.Hour)
	sessionReset := f.now.Add(2 * time.Hour)
	set := func(until time.Time) {
		a, _ := f.m.GetByID("a")
		// Only the credential-wide cooldown blocks; the models themselves are open.
		a.ModelStates = map[string]*ModelState{"claude-opus-5-5": {Status: StatusActive}, "claude-fable-5-2": {Status: StatusActive}}
		a.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: until}
		a.Unavailable, a.NextRetryAfter, a.LastError = true, until, &Error{HTTPStatus: 429}
		if _, err := f.m.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	r := ClaudeResetRecovery{CredentialID: "a", AccountUUID: "acct-a", OrganizationUUID: f.client.identity["a"][1], ClearsFable: true,
		FableResetAt: fableReset, WeeklyOpen: true, FableOpen: true, ObservedAt: f.now, ConfirmedAt: f.now}
	// A refusal of the 5-hour and Fable windows set the credential-wide
	// cooldown to the later, Fable deadline; the 5-hour window renewed since.
	set(fableReset.Add(40 * time.Second))
	if err := f.m.RecoverClaudeReset(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if a, _ := f.m.GetByID("a"); a.Unavailable || a.Quota.Exceeded {
		t.Fatalf("a Fable reset left the credential blocked: unavailable %v quota %+v", a.Unavailable, a.Quota)
	}
	// A credential-wide cooldown that may still belong to a full 5-hour window stays.
	set(sessionReset.Add(40 * time.Second))
	r.SessionFull, r.SessionResetAt = true, sessionReset
	if err := f.m.RecoverClaudeReset(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if a, _ := f.m.GetByID("a"); !a.Unavailable {
		t.Fatal("cleared a credential-wide cooldown of the full 5-hour window")
	}
}

func TestRecoverClaudeResetForgetsOnlyClearedWindow(t *testing.T) {
	f := newClaudeResetFixture(t)
	sel := &forgetWindowsSelector{}
	f.m.SetSelector(sel)
	base := ClaudeResetRecovery{CredentialID: "a", AccountUUID: "acct-a", OrganizationUUID: f.client.identity["a"][1], ConfirmedAt: f.now, ObservedAt: f.now, WeeklyOpen: true, FableOpen: true}
	weeklyOnly := base
	weeklyOnly.ClearsWeekly = true
	if err := f.m.RecoverClaudeReset(context.Background(), weeklyOnly); err != nil {
		t.Fatal(err)
	}
	if len(sel.windows) != 0 {
		t.Fatalf("weekly-only reset forgot %v", sel.windows)
	}
	fable := base
	fable.ClearsFable = true
	if err := f.m.RecoverClaudeReset(context.Background(), fable); err != nil {
		t.Fatal(err)
	}
	if len(sel.windows) != 1 || sel.windows[0] != claudeFableWindow {
		t.Fatalf("forgot %v, want the Fable window", sel.windows)
	}
}

func TestSettledComparesEachWindowWithItsOwnBaseline(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	reset := now.Add(48 * time.Hour)
	op := claudereset.Operation{GrantID: "g", ClearsWeekly: true, ClearsFable: true, ResetsLeftBefore: 1,
		WeeklyBefore: &claudereset.WindowBaseline{Used: 100, ResetAt: reset}, FableBefore: &claudereset.WindowBaseline{Used: 20, ResetAt: reset}}
	if _, known := settled(op, claudereset.Observation{}, now); known {
		t.Fatal("vanished grant settled without a reported window")
	}
	unchanged := claudereset.Observation{Weekly: claudereset.Window{Known: true, Used: 100}, Fable: claudereset.Window{Known: true, Used: 20}}
	if _, known := settled(op, unchanged, now); known {
		t.Fatal("unchanged windows taken as a spent reset (Fable compared with the weekly baseline)")
	}
	dropped := claudereset.Observation{Weekly: claudereset.Window{Known: true, Used: 1}, Fable: claudereset.Window{Known: true, Used: 20}}
	if consumed, known := settled(op, dropped, now); !known || !consumed {
		t.Fatal("dropped weekly usage not accepted as a spent reset")
	}
	if _, known := settled(op, dropped, reset.Add(time.Hour)); known {
		t.Fatal("a natural renewal was taken as a spent reset")
	}
}

func TestClaudeResetPassiveSignalsNeverDriveClaims(t *testing.T) {
	f := newClaudeResetFixture(t)
	// c's verified inventory shows allowance left; a later passive header says
	// it is used up. Only verified evidence may justify a spend.
	f.client.statuses["c"] = claudeResetObs(40, f.now.Add(72*time.Hour), 1)
	f.c.refresh(context.Background(), f.c.claudeAuths(), func(claudeResetStatusEntry) bool { return true })
	c, _ := f.m.GetByID("c")
	c.Quota.ObservedAt = f.now.Add(time.Second)
	c.Quota.Signals = map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "1", "Anthropic-Ratelimit-Unified-7d-Status": "rejected"}
	if _, err := f.m.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	f.client.results = []string{"reset"}
	f.c.tick(context.Background())
	if len(f.client.claims) != 0 {
		t.Fatalf("passive headers drove a claim: %v", f.client.claims)
	}
}

func TestClaudeResetGateClosedAfterJournalMarksNotSent(t *testing.T) {
	f := newClaudeResetFixture(t)
	saves := 0
	f.c.opts.Gate = func() (bool, string) {
		// Open through planning; closed once the attempt is journaled.
		if len(f.ops(t)) > 0 {
			saves++
			return false, "not_writer"
		}
		return true, ""
	}
	f.client.results = []string{"reset"}
	f.c.tick(context.Background())
	if len(f.client.claims) != 0 {
		t.Fatalf("claim sent after the gate closed: %v", f.client.claims)
	}
	op := f.op(t, "b")
	if op.State != claudereset.OpFailed || op.Result != claudereset.ResultNotSent || saves == 0 {
		t.Fatalf("op = %+v", op)
	}
	if h := claudereset.Summarize(f.ops(t), f.now); !h.BackoffUntil[claudereset.AccountKey("acct-b", "b")].IsZero() {
		t.Fatalf("an unsent claim paused the account: %+v", h)
	}
}

func TestStopClaudeResetControllerDrains(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.StartClaudeResetController(context.Background(), &fakeClaudeResetClient{}, ClaudeResetOptions{
		Gate:     func() (bool, string) { return false, "not_writer" },
		Lock:     func() (func(), error) { return func() {}, nil },
		StateDir: func() string { return t.TempDir() },
	})
	m.claudeResetMu.Lock()
	c := m.claudeResets
	m.claudeResetMu.Unlock()
	m.StopClaudeResetController()
	select {
	case <-c.done:
	default:
		t.Fatal("controller still running after stop")
	}
}

// A forced account whose weekly cooldown was restored after a restart makes
// the policy selector refuse with account_policy_unavailable. That refusal,
// through the real Manager.Execute path and with no preloaded need, must arm
// the forced account's weekly reset.
func TestClaudeResetForcedAccountCooldownThroughExecute(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	model := "claude-opus-5-5"
	reset := now.Add(96 * time.Hour)
	executor := &claudeCancellationTestExecutor{}
	m := NewManager(nil, nil, NoopHook{})
	m.SetRetryConfig(0, 0, 0)
	m.RegisterExecutor(executor)
	ids := map[string]string{"a": "claude-forced-a-" + uuid.NewString(), "b": "claude-forced-b-" + uuid.NewString()}
	client := &fakeClaudeResetClient{statuses: map[string]claudereset.Observation{}, identity: map[string][2]string{}, statusReads: map[string]int{},
		failStatus: map[string]bool{}, stale: map[string]bool{}, staleFable: map[string]bool{}}
	for i, key := range []string{"a", "b"} {
		id := ids[key]
		account, org := "acct-"+key, "22222222-2222-2222-2222-22222222222"+string(rune('1'+i))
		a := &Auth{ID: id, Provider: "claude", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"},
			Metadata: map[string]any{"access_token": "tok-" + key, "account_uuid": account, "organization_uuid": org, "request_retry": float64(0)}}
		if key == "a" {
			a.ModelStates = map[string]*ModelState{model: quotaCooldown(reset.Add(40 * time.Second))}
		}
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		client.identity[id] = [2]string{account, org}
	}
	client.statuses[ids["a"]] = claudeResetObs(100, reset, 1)
	client.statuses[ids["b"]] = claudeResetObs(10, reset, 1)
	settings := accountpolicy.DefaultSettings()
	settings.Enabled = true
	settings.ForceAccount = ids["a"]
	sel := NewEarliestDeadlineSelector(&policyTestSource{settings: settings, snapshots: map[string]accountpolicy.Snapshot{}}, &FillFirstSelector{})
	sel.SetBindingAuthResolver(m.GetByID)
	t.Cleanup(sel.Stop)
	m.SetSelector(sel)
	c := newClaudeResetController(m, client, ClaudeResetOptions{
		Gate:     func() (bool, string) { return true, "" },
		Lock:     func() (func(), error) { return func() {}, nil },
		StateDir: func() string { return t.TempDir() },
		Held:     func(string) bool { return false },
		Forced:   func() string { return ids["a"] },
		Now:      func() time.Time { return now },
	})
	m.claudeResetMu.Lock()
	m.claudeResets = c
	m.claudeResetMu.Unlock()
	c.refresh(context.Background(), c.claudeAuths(), func(claudeResetStatusEntry) bool { return true })

	if _, err := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("Execute served while the forced account is cooling down")
	}
	if executor.executeCalls.Load() != 0 {
		t.Fatalf("an excluded account served the request: %d calls", executor.executeCalls.Load())
	}
	if r := m.claudeRefusalTimes(); r.localWeekly.IsZero() {
		t.Fatalf("the forced account's weekly refusal was not recorded: %+v", r)
	}
	client.results = []string{"reset"}
	c.tick(context.Background())
	if len(client.claims) != 1 || !strings.HasPrefix(client.claims[0], ids["a"]+"/") {
		t.Fatalf("claims = %v, want the forced account", client.claims)
	}
}
