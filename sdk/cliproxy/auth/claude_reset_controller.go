package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudereset"
	log "github.com/sirupsen/logrus"
)

// The Claude reset controller spends Claude subscription reset grants
// automatically; internal/claudereset holds the decision rules. It reads
// provider state freely, but every journal write and provider write happens
// while it holds the account-policy redemption lock (the deployment fence) and
// the writer gate is open. Each claim is journaled before it is sent.
const (
	claudeResetTick         = time.Minute
	claudeResetStartDelay   = 90 * time.Second
	claudeResetInventoryAge = time.Hour
	// claudeResetNearAge refreshes inventory this often while a trigger is close.
	claudeResetNearAge = 5 * time.Minute
	// claudeResetNearMargin widens the need threshold for early refreshes.
	claudeResetNearMargin = 20.0
	// claudeResetClaimAge is the maximum age of the target's inventory at claim.
	claudeResetClaimAge = 90 * time.Second
	// claudeResetCallTimeout bounds one control-plane call; it never applies to
	// inference traffic.
	claudeResetCallTimeout = 30 * time.Second
	claudeResetRetryEvery  = time.Minute
	// claudeResetDropPoints is the usage drop that shows a vanished grant was
	// spent rather than expired.
	claudeResetDropPoints = 10.0
)

// ClaudeResetClient talks to Anthropic's reset program for one account.
type ClaudeResetClient interface {
	// Status reads usage and the reset inventory, plus the same usage rendered
	// as unified rate-limit headers for the passive observation path.
	Status(ctx context.Context, a *Auth, accessToken string) (claudereset.Observation, http.Header, error)
	// Identity returns the token's account and organization UUIDs.
	Identity(ctx context.Context, a *Auth, accessToken string) (accountUUID, organizationUUID string, err error)
	// Claim spends one reset. A nil error carries the provider's result; any
	// error means the outcome is unknown.
	Claim(ctx context.Context, a *Auth, accessToken, organizationUUID, grantID, requestID string) (string, error)
}

// ClaudeResetOptions connects the controller to its host.
type ClaudeResetOptions struct {
	// Gate reports whether claims are allowed, and a reason when they are not.
	// "off" means the operator disabled automatic Claude resets.
	Gate func() (bool, string)
	// Lock acquires the redemption lock shared with the deployment runner and
	// account-policy redemption; it fails while another holder owns it.
	Lock func() (func(), error)
	// StateDir returns the account-policy state directory for the journal.
	StateDir func() string
	// Held reports operator holds; held accounts never serve restored allowance.
	Held func(id string) bool
	// Forced returns the credential routing is forced to, empty when none;
	// while set, no other account serves.
	Forced func() string
	// Now overrides the clock in tests.
	Now func() time.Time
}

type claudeResetController struct {
	m      *Manager
	client ClaudeResetClient
	opts   ClaudeResetOptions
	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}

	mu          sync.Mutex
	pool        string
	poolForced  string
	poolKnown   bool
	recoveries  map[string]*claudeRecoveryProgress
	journal     *claudereset.Journal
	journalErr  string
	statuses    map[string]claudeResetStatusEntry
	bearers     map[string]claudeResetBearer
	plan        claudereset.Plan
	evaluatedAt time.Time
	allowed     bool
	gateReason  string
}

// claudeResetStatusEntry is one inventory read, bound to the registration,
// stored identity and verified bearer token it was read with.
type claudeResetStatusEntry struct {
	epoch   uint64
	account string
	org     string
	bearer  string
	obs     claudereset.Observation
	err     string
	tried   time.Time
}

// claudeRecoveryProgress is one spent reset's recovery in this process: the
// execution fence is taken once, and each routing recovery step runs once.
type claudeRecoveryProgress struct {
	fenced      bool
	weekly      bool
	fableModels bool
}

// claudeResetBearer is the provider-reported identity of one access token.
type claudeResetBearer struct {
	account string
	org     string
}

// StartClaudeResetController starts automatic Claude reset redemption.
func (m *Manager) StartClaudeResetController(ctx context.Context, client ClaudeResetClient, opts ClaudeResetOptions) {
	if m == nil || client == nil || opts.Gate == nil || opts.Lock == nil || opts.StateDir == nil {
		return
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Held == nil {
		opts.Held = func(string) bool { return false }
	}
	if opts.Forced == nil {
		opts.Forced = func() string { return "" }
	}
	runCtx, cancel := context.WithCancel(ctx)
	c := newClaudeResetController(m, client, opts)
	c.cancel, c.done = cancel, make(chan struct{})
	m.claudeResetMu.Lock()
	previous := m.claudeResets
	m.claudeResets = c
	m.claudeResetMu.Unlock()
	if previous != nil {
		previous.stop()
	}
	go func() {
		defer close(c.done)
		timer := time.NewTimer(claudeResetStartDelay)
		defer timer.Stop()
		select {
		case <-runCtx.Done():
			return
		case <-timer.C:
		}
		ticker := time.NewTicker(claudeResetTick)
		defer ticker.Stop()
		for {
			c.tick(runCtx)
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			case <-c.wake:
			}
		}
	}()
}

func newClaudeResetController(m *Manager, client ClaudeResetClient, opts ClaudeResetOptions) *claudeResetController {
	return &claudeResetController{m: m, client: client, opts: opts, statuses: map[string]claudeResetStatusEntry{},
		bearers: map[string]claudeResetBearer{}, recoveries: map[string]*claudeRecoveryProgress{}, wake: make(chan struct{}, 1)}
}

// StopClaudeResetController stops the controller and waits for an in-flight
// claim to finish, so writer ownership is never released under a claim.
func (m *Manager) StopClaudeResetController() {
	if m == nil {
		return
	}
	m.claudeResetMu.Lock()
	c := m.claudeResets
	m.claudeResetMu.Unlock()
	if c != nil {
		c.stop()
	}
}

func (c *claudeResetController) stop() {
	if c.cancel != nil {
		c.cancel()
	}
	if c.done != nil {
		<-c.done
	}
}

// noteClaudeWindowRefusal remembers, on the logical request, a Claude attempt
// that Anthropic refused for the weekly or the Fable window (from the failed
// result's rate-limit headers). It becomes need only if the request finally
// fails; another account serving the retry means the fleet served it. Other
// refusals (5-hour window, entitlement or request-scoped limits) never count.
func (m *Manager) noteClaudeWindowRefusal(result Result, headers http.Header) {
	if m == nil || result.Success || len(headers) == 0 || !strings.EqualFold(strings.TrimSpace(result.Provider), "claude") {
		return
	}
	lease, ok := result.Options.Metadata[policyRequestLeaseMetadataKey].(*policyRequestLease)
	if !ok || lease == nil {
		return
	}
	rejected := func(window string) bool {
		return strings.EqualFold(strings.TrimSpace(headers.Get("Anthropic-Ratelimit-Unified-"+window+"-Status")), "rejected")
	}
	// The result names the resolved model, so an alias route keeps its Fable
	// attribution; 7d_oi on a non-Fable model (an overage cap) is not Fable.
	weekly, fable := rejected("7d"), rejected(claudeFableWindow) && claudeFableModel(result.Model)
	if !weekly && !fable {
		return
	}
	lease.mu.Lock()
	lease.claudeWeeklyRefused = lease.claudeWeeklyRefused || weekly
	lease.claudeFableRefused = lease.claudeFableRefused || fable
	lease.mu.Unlock()
}

// noteClaudeRequestOutcome publishes Claude reset need when a request on a
// Claude route finally failed with a refusal: the window refusals its attempts
// collected, and a local refusal because every credential was cooling down.
// A local refusal counts only when the controller confirms that the cooldowns
// come from exhausted weekly or Fable windows. It wakes the controller.
func (m *Manager) noteClaudeRequestOutcome(providers []string, model string, err error, lease *policyRequestLease) {
	if m == nil || err == nil {
		return
	}
	var cooldown *modelCooldownError
	var selection *Error
	// Selection refused the route: every credential cooling down, possibly
	// alongside ones that cannot serve at all (auth_unavailable).
	// account_policy_unavailable: the policy selector found no eligible
	// account, for example a forced account whose cooldown was restored.
	local := (errors.As(err, &cooldown) && cooldown != nil) ||
		(errors.As(err, &selection) && selection != nil &&
			(selection.Code == "auth_unavailable" || selection.Code == "model_cooldown" || selection.Code == "account_policy_unavailable"))
	if !local && statusCodeFromError(err) != 429 {
		return
	}
	claude := false
	for _, provider := range providers {
		claude = claude || strings.EqualFold(strings.TrimSpace(provider), "claude")
	}
	if !claude {
		return
	}
	key := strings.ToLower(canonicalModelKey(model))
	// Window refusals were attributed from Claude results with their resolved
	// models, so an alias route keeps them.
	weekly, fable := false, false
	if lease != nil {
		lease.mu.Lock()
		weekly, fable = lease.claudeWeeklyRefused, lease.claudeFableRefused
		lease.mu.Unlock()
	}
	if !weekly && !fable && !local {
		return
	}
	// A local refusal's cause is judged now, against the fleet at refusal
	// time; later changes cannot give it a weekly or Fable cause.
	localWeekly, localFable := false, false
	m.claudeResetMu.Lock()
	c := m.claudeResets
	m.claudeResetMu.Unlock()
	if local && c != nil {
		localWeekly, localFable = c.localRefusalCauses()
		localFable = localFable && m.claudeFableRoute(key)
	}
	if c == nil {
		return
	}
	if (!weekly && !fable && !localWeekly && !localFable) || lease == nil || !lease.claudePoolKnown {
		// Without a confirmed cause or a known routing pool this is not need,
		// but fresher inventory may confirm the next refusal: wake the controller.
		c.poke()
		return
	}
	m.recordClaudeRefusals(c, time.Now().UTC(), lease.claudePool, weekly, fable, localWeekly, localFable)
}

// claudeRefusalPool returns the routing pool of the controller's latest
// evaluation. A request captures it when it starts, so a refusal is bound to
// the pool it was routed under even if an override changes while it runs.
func (m *Manager) claudeRefusalPool() (string, bool) {
	if m == nil {
		return "", false
	}
	m.claudeResetMu.Lock()
	c := m.claudeResets
	m.claudeResetMu.Unlock()
	if c == nil {
		return "", false
	}
	return c.currentPool()
}

// forced returns the force-account override, or "".
func (c *claudeResetController) forced() string {
	if c.opts.Forced == nil {
		return ""
	}
	return c.opts.Forced()
}

// currentPool returns the cached routing pool, recomputing it when none is
// known yet or the force-account override changed since it was computed.
func (c *claudeResetController) currentPool() (string, bool) {
	forced := c.forced()
	c.mu.Lock()
	pool, known, cachedForced := c.pool, c.poolKnown, c.poolForced
	c.mu.Unlock()
	if known && cachedForced == forced {
		return pool, true
	}
	return c.storePool(forced, c.poolKey(c.claudeAuths())), true
}

func (c *claudeResetController) storePool(forced, pool string) string {
	c.mu.Lock()
	c.pool, c.poolForced, c.poolKnown = pool, forced, true
	c.mu.Unlock()
	return pool
}

// recordClaudeRefusals stores confirmed need with the routing pool (the
// accounts that could serve) the request was routed under; it counts only
// while that pool is unchanged, so a force-account or hold change cannot hand
// one pool's refusal to accounts that served all along. It wakes the
// controller.
func (m *Manager) recordClaudeRefusals(c *claudeResetController, at time.Time, pool string, weekly, fable, localWeekly, localFable bool) {
	refusal := claudeRefusal{at: at, pool: pool}
	m.claudeResetMu.Lock()
	if weekly {
		m.claudeRefusals.weekly = refusal
	}
	if fable {
		m.claudeRefusals.fable = refusal
	}
	if localWeekly {
		m.claudeRefusals.localWeekly = refusal
	}
	if localFable {
		m.claudeRefusals.localFable = refusal
	}
	m.claudeResetMu.Unlock()
	c.poke()
}

// poolKey identifies the accounts that can serve now (healthy, not held, not
// excluded by force-account).
func (c *claudeResetController) poolKey(auths []*Auth) string {
	ids := make([]string, 0, len(auths))
	for _, a := range auths {
		if c.healthy(a) {
			// The logical identity keeps a same-ID replacement out of the pool.
			account, org := claudeResetIdentity(a)
			ids = append(ids, a.ID+"|"+account+"|"+org)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, "\x00")
}

// claudeFableRoute reports whether route resolves to a Fable model on any
// Claude credential, so a neutral alias (for example "fable") keeps its Fable
// attribution on a local refusal that happened before any upstream attempt.
func (m *Manager) claudeFableRoute(route string) bool {
	if claudeFableModel(route) {
		return true
	}
	for _, a := range m.List() {
		if isClaudeAuth(a) && claudeFableModel(m.selectionModelKeyForAuth(a, route)) {
			return true
		}
	}
	return false
}

func (c *claudeResetController) poke() {
	if c == nil {
		return
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// claudeRefusal is confirmed need and the routing pool it was refused for.
type claudeRefusal struct {
	at   time.Time
	pool string
}

type claudeRefusals struct {
	weekly, fable, localWeekly, localFable claudeRefusal
}

func (m *Manager) claudeRefusalTimes() claudeRefusals {
	m.claudeResetMu.Lock()
	defer m.claudeResetMu.Unlock()
	return m.claudeRefusals
}

func (c *claudeResetController) now() time.Time { return c.opts.Now().UTC() }

func (c *claudeResetController) tick(ctx context.Context) {
	allowed, reason := c.opts.Gate()
	c.mu.Lock()
	c.allowed, c.gateReason = allowed, reason
	c.mu.Unlock()
	if allowed && !c.ensureJournal() {
		allowed = false
	}

	auths := c.claudeAuths()
	now := c.now()
	c.refresh(ctx, auths, func(e claudeResetStatusEntry) bool { return now.Sub(e.obs.ObservedAt) >= claudeResetInventoryAge })
	plan := c.evaluate(auths, "")
	// Passive usage only decides when to read early; decisions use verified reads.
	if allowed && (c.near(plan) || c.near(c.estimate(auths))) {
		now = c.now()
		c.refresh(ctx, auths, func(e claudeResetStatusEntry) bool { return now.Sub(e.obs.ObservedAt) >= claudeResetNearAge })
		plan = c.evaluate(auths, "")
	}
	if !allowed || ctx.Err() != nil || (plan.Claim == nil && !c.hasJournalWork()) {
		return
	}
	unlock, err := c.opts.Lock()
	if err != nil {
		log.Debugf("claude reset: redemption lock unavailable: %v", err)
		return
	}
	defer unlock()
	if !c.gateOpen() || !c.reloadJournal() {
		return
	}
	c.recoverLocal(ctx)
	c.recoverPending(ctx)
	plan = c.evaluate(c.claudeAuths(), "")
	if plan.Claim == nil || ctx.Err() != nil {
		return
	}
	c.claim(ctx, *plan.Claim)
}

func (c *claudeResetController) gateOpen() bool {
	allowed, _ := c.opts.Gate()
	return allowed
}

// reloadJournal re-reads the journal under the redemption lock, so decisions
// see every operation, including one another process journaled.
func (c *claudeResetController) reloadJournal() bool {
	c.mu.Lock()
	j := c.journal
	c.mu.Unlock()
	if j == nil {
		return false
	}
	if err := j.Reload(); err != nil {
		log.WithError(err).Error("claude reset: journal reload failed; claims paused")
		return false
	}
	return true
}

// hasJournalWork reports unresolved claims or unfinished local recoveries.
func (c *claudeResetController) hasJournalWork() bool {
	now := c.now()
	for _, op := range c.operations() {
		if op.State == claudereset.OpPending || (op.State == claudereset.OpDone && !op.Recovered && now.Before(claudereset.RecoveryUntil(op))) {
			return true
		}
	}
	return false
}

// near reports whether a trigger may fire soon, which justifies fresher reads.
func (c *claudeResetController) near(plan claudereset.Plan) bool {
	if plan.Claim != nil || c.hasJournalWork() {
		return true
	}
	limit := claudereset.HeadroomThreshold + claudeResetNearMargin
	if plan.Weekly.Headroom < limit || plan.Fable.Headroom < limit {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, e := range c.statuses {
		for _, g := range e.obs.Grants {
			if !g.EndsAt.IsZero() && g.ResetsLeft > 0 && g.EndsAt.Sub(now) <= claudereset.ExpiryGuard {
				return true
			}
		}
	}
	return false
}

func (c *claudeResetController) ensureJournal() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.journal != nil {
		return true
	}
	j, err := claudereset.OpenJournal(c.opts.StateDir())
	if err != nil {
		if c.journalErr == "" {
			log.WithError(err).Error("claude reset: journal unavailable; automatic resets are paused")
		}
		c.journalErr = "journal unavailable"
		return false
	}
	c.journal, c.journalErr = j, ""
	return true
}

func (c *claudeResetController) operations() []claudereset.Operation {
	c.mu.Lock()
	j := c.journal
	c.mu.Unlock()
	if j == nil {
		return nil
	}
	return j.Operations()
}

func (c *claudeResetController) saveOp(op claudereset.Operation) error {
	c.mu.Lock()
	j := c.journal
	c.mu.Unlock()
	if j == nil {
		return errors.New("journal unavailable")
	}
	return j.Save(op)
}

func (c *claudeResetController) claudeAuths() []*Auth {
	var out []*Auth
	for _, a := range c.m.List() {
		if isClaudeAuth(a) {
			out = append(out, a)
		}
	}
	return out
}

func (c *claudeResetController) healthy(a *Auth) bool {
	if a == nil || a.Disabled || a.Status == StatusDisabled || hasUnauthorizedAuthFailure(a) || c.opts.Held(a.ID) {
		return false
	}
	if c.opts.Forced != nil {
		if forced := c.opts.Forced(); forced != "" && forced != a.ID {
			return false
		}
	}
	return true
}

func claudeResetIdentity(a *Auth) (string, string) {
	account, _ := a.Metadata["account_uuid"].(string)
	org, _ := a.Metadata["organization_uuid"].(string)
	return strings.ToLower(strings.TrimSpace(account)), strings.ToLower(strings.TrimSpace(org))
}

func claudeResetBearerKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}

// refresh reads the inventory of healthy accounts whose entry is due.
func (c *claudeResetController) refresh(ctx context.Context, auths []*Auth, due func(claudeResetStatusEntry) bool) {
	for _, a := range auths {
		if ctx.Err() != nil || !c.healthy(a) {
			continue
		}
		account, org := claudeResetIdentity(a)
		c.mu.Lock()
		e, ok := c.statuses[a.ID]
		c.mu.Unlock()
		retryAfter := time.Minute
		if e.err != "" {
			retryAfter = claudeResetNearAge
		}
		same := ok && e.epoch == a.RegistrationEpoch && e.account == account && e.org == org && e.bearer == claudeResetBearerKey(authAccessToken(a))
		if same && (!due(e) || c.now().Sub(e.tried) < retryAfter) {
			continue
		}
		c.readStatus(ctx, a, "", "")
	}
}

// bearerIdentity returns the provider identity of token, cached per token.
func (c *claudeResetController) bearerIdentity(ctx context.Context, a *Auth, token string) (claudeResetBearer, bool) {
	key := claudeResetBearerKey(token)
	c.mu.Lock()
	cached, ok := c.bearers[key]
	c.mu.Unlock()
	if ok {
		return cached, true
	}
	callCtx, cancel := context.WithTimeout(ctx, claudeResetCallTimeout)
	account, org, err := c.client.Identity(callCtx, a, token)
	cancel()
	if err != nil {
		return claudeResetBearer{}, false
	}
	b := claudeResetBearer{account: strings.ToLower(strings.TrimSpace(account)), org: strings.ToLower(strings.TrimSpace(org))}
	c.mu.Lock()
	if len(c.bearers) >= 64 {
		c.bearers = map[string]claudeResetBearer{}
	}
	c.bearers[key] = b
	c.mu.Unlock()
	return b, true
}

// readStatus reads a's inventory with its current token. It succeeds only when
// the token's provider identity matches the stored credential identity (and,
// when given, the expected one), so the inventory is decision-grade for that
// account. Reads of other identities are discarded.
func (c *claudeResetController) readStatus(ctx context.Context, a *Auth, wantAccount, wantOrg string) (claudereset.Observation, bool) {
	started := c.now()
	account, org := claudeResetIdentity(a)
	c.mu.Lock()
	e := c.statuses[a.ID]
	if e.epoch != a.RegistrationEpoch || e.account != account || e.org != org {
		e = claudeResetStatusEntry{epoch: a.RegistrationEpoch, account: account, org: org}
	}
	e.tried = started
	c.statuses[a.ID] = e
	c.mu.Unlock()
	token := authAccessToken(a)
	if exp, ok := a.AccessTokenExpirationTime(); token == "" || account == "" || org == "" || (ok && !exp.IsZero() && !exp.After(started)) {
		c.setStatusError(a.ID, "access token or identity unavailable")
		return claudereset.Observation{}, false
	}
	if wantAccount != "" && (account != wantAccount || org != wantOrg) {
		c.setStatusError(a.ID, "credential identity changed")
		return claudereset.Observation{}, false
	}
	bearer, ok := c.bearerIdentity(ctx, a, token)
	if !ok || bearer.account != account || bearer.org != org {
		c.setStatusError(a.ID, "token identity does not match the credential")
		return claudereset.Observation{}, false
	}
	callCtx, cancel := context.WithTimeout(ctx, claudeResetCallTimeout)
	obs, headers, err := c.client.Status(callCtx, a, token)
	cancel()
	if err != nil {
		log.Debugf("claude reset: inventory read failed for %s: %v", a.ID, err)
		c.setStatusError(a.ID, "inventory read failed")
		return claudereset.Observation{}, false
	}
	obs.ObservedAt = started
	for _, w := range []*claudereset.Window{&obs.Weekly, &obs.Fable, &obs.Session} {
		if w.Known {
			w.ObservedAt = started
		}
	}
	c.mu.Lock()
	c.statuses[a.ID] = claudeResetStatusEntry{epoch: a.RegistrationEpoch, account: account, org: org, bearer: claudeResetBearerKey(token), obs: obs, tried: started}
	c.mu.Unlock()
	if len(headers) > 0 {
		c.m.observeClaudeUsage(a.ID, a.RegistrationEpoch, headers, started)
	}
	return obs, true
}

func (c *claudeResetController) setStatusError(id, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.statuses[id]
	e.err = msg
	c.statuses[id] = e
}

// verifiedEntry returns a's inventory when it was read with the credential's
// current token, registration and identity, and that token verified as that
// account. Anything else is not decision-grade evidence.
func (c *claudeResetController) verifiedEntry(a *Auth) (claudeResetStatusEntry, bool) {
	account, org := claudeResetIdentity(a)
	e := c.statuses[a.ID]
	if e.obs.ObservedAt.IsZero() || e.epoch != a.RegistrationEpoch || e.account != account || e.org != org || e.bearer != claudeResetBearerKey(authAccessToken(a)) {
		return claudeResetStatusEntry{}, false
	}
	if b, ok := c.bearers[e.bearer]; !ok || b.account != account || b.org != org {
		return claudeResetStatusEntry{}, false
	}
	return e, true
}

// accounts builds the decision view: verified inventory, operator holds and
// journal history. withSignals also overlays unverified passive usage; that
// view only decides when to read early, never what to spend.
func (c *claudeResetController) accounts(auths []*Auth, history claudereset.History, withSignals bool) []claudereset.Account {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]claudereset.Account, 0, len(auths))
	for _, a := range auths {
		account, _ := claudeResetIdentity(a)
		acct := claudereset.Account{ID: a.ID, Healthy: c.healthy(a)}
		for _, key := range []string{claudereset.AccountKey(account, a.ID), claudereset.AccountKey("", a.ID)} {
			if history.Pending[key] {
				acct.Pending = true
			}
			if history.Unrecovered[key] {
				acct.Unrecovered = true
			}
			if history.BackoffUntil[key].After(acct.BackoffUntil) {
				acct.BackoffUntil = history.BackoffUntil[key]
			}
			if history.LastClaimAt[key].After(acct.LastClaimAt) {
				acct.LastClaimAt = history.LastClaimAt[key]
			}
		}
		if e, ok := c.verifiedEntry(a); ok {
			acct.Observation = e.obs
		}
		if withSignals {
			mergeClaudeSignals(&acct, a)
		}
		out = append(out, acct)
	}
	return out
}

// evaluate decides on verified evidence and records the plan. ignoreOp leaves
// one journaled operation out, for re-deciding that unresolved claim.
func (c *claudeResetController) evaluate(auths []*Auth, ignoreOp string) claudereset.Plan {
	in := c.input(auths, ignoreOp, false)
	plan := claudereset.Decide(in)
	if ignoreOp == "" {
		c.mu.Lock()
		c.plan, c.evaluatedAt = plan, in.Now
		c.mu.Unlock()
	}
	return plan
}

// estimate decides on verified and passive evidence, only to pace reads.
func (c *claudeResetController) estimate(auths []*Auth) claudereset.Plan {
	return claudereset.Decide(c.input(auths, "", true))
}

func (c *claudeResetController) input(auths []*Auth, ignoreOp string, withSignals bool) claudereset.Input {
	now := c.now()
	ops := c.operations()
	if ignoreOp != "" {
		kept := ops[:0]
		for _, op := range ops {
			if op.ID != ignoreOp {
				kept = append(kept, op)
			}
		}
		ops = kept
	}
	history := claudereset.Summarize(ops, now)
	in := claudereset.Input{Now: now, LastNeedClaimAt: history.LastNeedClaimAt, NeedClaims24h: history.NeedClaims24h}
	in.Accounts = c.accounts(auths, history, withSignals)
	// Need is a refusal Anthropic attributed to the window, or a local refusal
	// while every healthy account is blocked by that window, and only for the
	// routing pool it was refused for.
	r := c.m.claudeRefusalTimes()
	forced := c.forced()
	pool := c.storePool(forced, c.poolKey(auths))
	latest := func(refusals ...claudeRefusal) time.Time {
		var at time.Time
		for _, refusal := range refusals {
			if refusal.pool == pool && refusal.at.After(at) {
				at = refusal.at
			}
		}
		return at
	}
	in.ClaudeDemandAt = latest(r.weekly, r.localWeekly)
	in.FableDemandAt = latest(r.fable, r.localFable)
	return in
}

// localRefusalCauses reports whether a local refusal right now is caused by
// exhausted weekly windows, or by exhausted weekly or Fable windows, across
// every healthy account (verified inventory or cooldowns ending at the
// window's reset).
func (c *claudeResetController) localRefusalCauses() (weekly, fable bool) {
	auths := c.claudeAuths()
	now := c.now()
	// Classification needs only inventory and cooldowns, not the journal.
	accts := c.accounts(auths, claudereset.History{}, false)
	return fleetCooling(auths, accts, false, now), fleetCooling(auths, accts, true, now)
}

// windowOut reports a window the evidence shows used up.
func windowOut(obs claudereset.Observation, name string, w claudereset.Window) bool {
	if w.Known && w.Used >= 100 {
		return true
	}
	for _, exhausted := range obs.Exhausted {
		if exhausted == name {
			return true
		}
	}
	return false
}

// cooldownCausedBy reports an active quota cooldown on a that ends at reset:
// the credential-wide one, or a model's (Fable models only when fable).
func cooldownCausedBy(a *Auth, reset time.Time, fable bool, now time.Time) bool {
	if reset.IsZero() {
		return false
	}
	active := func(q QuotaState, retry time.Time, lastErr *Error) bool {
		until := quotaCooldownUntil(q, retry)
		return policyQuotaFailure(q, lastErr) && until.After(now) && causedBy(until, reset)
	}
	if !fable && active(a.Quota, a.NextRetryAfter, a.LastError) {
		return true
	}
	for model, state := range a.ModelStates {
		if state != nil && (!fable || claudeFableModel(model)) && active(state.Quota, state.NextRetryAfter, state.LastError) {
			return true
		}
	}
	return false
}

// weeklyCooldown reports an active quota cooldown that only the weekly window
// can explain: it ends at the weekly reset and not at the Fable reset.
func weeklyCooldown(a *Auth, acct claudereset.Account, now time.Time) bool {
	if acct.Fable.ResetAt.IsZero() || causedBy(acct.Weekly.ResetAt, acct.Fable.ResetAt) || causedBy(acct.Fable.ResetAt, acct.Weekly.ResetAt) {
		return false
	}
	return cooldownCausedBy(a, acct.Weekly.ResetAt, false, now)
}

// fleetCooling reports whether every healthy account is blocked by its weekly
// window (or, for fable, by its weekly or Fable window), according to verified
// inventory or a cooldown caused by that window.
func fleetCooling(auths []*Auth, accts []claudereset.Account, fable bool, now time.Time) bool {
	seen, restorable := false, false
	for i, a := range auths {
		acct := accts[i]
		if !acct.Healthy {
			continue
		}
		seen = true
		// Exhaustion evidence counts only when fresh and from the window that
		// is running now; a renewed or stale reading proves nothing. A cooldown
		// proves a weekly cause only when its deadline cannot also be the
		// Fable window's: the two usually renew together, and a Fable refusal
		// can block the whole credential while weekly allowance remains.
		weeklyOut := (acct.Weekly.Current(now) && windowOut(acct.Observation, claudereset.WindowWeekly, acct.Weekly)) || weeklyCooldown(a, acct, now)
		if weeklyOut {
			// A weekly reset restores this account for weekly need.
			restorable = restorable || !fable
			continue
		}
		// A verified full 5-hour window blocks the account as the decision
		// counts it, but no reset grant reopens it.
		if claudereset.SessionBlocked(acct, now) {
			continue
		}
		if fable && ((acct.Fable.Current(now) && windowOut(acct.Observation, claudereset.WindowFable, acct.Fable)) || cooldownCausedBy(a, acct.Fable.ResetAt, true, now)) {
			restorable = true
			continue
		}
		return false
	}
	// A fleet blocked only by 5-hour windows is not need: no grant helps.
	return seen && restorable
}

// mergeClaudeSignals overlays each usage window from response headers or the
// usage poller when that window was observed after the inventory. A header
// without a reset keeps the known reset time.
func mergeClaudeSignals(acct *claudereset.Account, a *Auth) {
	observed := a.Quota.ObservedAt
	if observed.IsZero() || len(a.Quota.Signals) == 0 {
		return
	}
	merge := func(name string, w *claudereset.Window) {
		if !observed.After(w.ObservedAt) {
			return
		}
		prefix := "Anthropic-Ratelimit-Unified-" + name + "-"
		raw := strings.TrimSpace(a.Quota.Signals[http.CanonicalHeaderKey(prefix+"Utilization")])
		status := strings.ToLower(strings.TrimSpace(a.Quota.Signals[http.CanonicalHeaderKey(prefix+"Status")]))
		used, errUsed := strconv.ParseFloat(raw, 64)
		switch {
		case errUsed == nil && !math.IsNaN(used) && !math.IsInf(used, 0) && used >= 0:
			used *= 100
		case status == "rejected":
			used = 100
		default:
			return
		}
		if status == "rejected" && used < 100 {
			used = 100
		}
		next := claudereset.Window{Known: true, Used: used, ResetAt: w.ResetAt, ObservedAt: observed}
		if seconds, errReset := strconv.ParseInt(strings.TrimSpace(a.Quota.Signals[http.CanonicalHeaderKey(prefix+"Reset")]), 10, 64); errReset == nil && seconds > 0 {
			next.ResetAt = time.Unix(seconds, 0).UTC()
		}
		*w = next
	}
	merge("7d", &acct.Weekly)
	merge(claudeFiveHourWindow, &acct.Session)
	merge(claudeFableWindow, &acct.Fable)
}

// claimToken returns a valid access token, refreshing it when it expires soon.
func (c *claudeResetController) claimToken(ctx context.Context, a *Auth) (*Auth, string, error) {
	if !a.HasValidAccessToken(c.now().Add(2 * time.Minute)) {
		refreshed, err := c.m.ForceRefreshAuth(ctx, a.ID)
		if err != nil {
			return nil, "", err
		}
		a = refreshed
	}
	token := authAccessToken(a)
	if token == "" {
		return nil, "", errors.New("access token unavailable")
	}
	return a, token, nil
}

// claimTarget is the credential, token and verified read a claim is bound to.
type claimTarget struct {
	auth  *Auth
	token string
	obs   claudereset.Observation
}

// freshTarget refreshes the token if it expires soon, then re-reads the
// inventory with it. The claim must use the same verified token, registration
// and identity as this read.
func (c *claudeResetController) freshTarget(ctx context.Context, id, wantAccount, wantOrg string) (claimTarget, bool) {
	return c.target(ctx, id, wantAccount, wantOrg, true)
}

// reconcileTarget is freshTarget for read-only reconciliation: operator
// holds and force-account restrict where claims are sent, not whether a
// verified read may settle an earlier claim.
func (c *claudeResetController) reconcileTarget(ctx context.Context, id, wantAccount, wantOrg string) (claimTarget, bool) {
	return c.target(ctx, id, wantAccount, wantOrg, false)
}

func (c *claudeResetController) target(ctx context.Context, id, wantAccount, wantOrg string, eligible bool) (claimTarget, bool) {
	a, ok := c.m.GetByID(id)
	if !ok || !isClaudeAuth(a) {
		return claimTarget{}, false
	}
	a, token, err := c.claimToken(ctx, a)
	if err != nil {
		return claimTarget{}, false
	}
	obs, ok := c.readStatus(ctx, a, wantAccount, wantOrg)
	if !ok {
		return claimTarget{}, false
	}
	t := claimTarget{auth: a, token: token, obs: obs}
	return t, c.unchanged(t, eligible)
}

// stillTarget reports whether the credential is still exactly as it was read
// and may receive a claim.
func (c *claudeResetController) stillTarget(t claimTarget) bool {
	return c.unchanged(t, true)
}

// unchanged reports whether the credential is still exactly as it was read;
// eligible also requires that it may receive a claim (healthy, not held, not
// excluded by force-account).
func (c *claudeResetController) unchanged(t claimTarget, eligible bool) bool {
	current, ok := c.m.GetByID(t.auth.ID)
	if !ok || current.RegistrationEpoch != t.auth.RegistrationEpoch || authAccessToken(current) != t.token || (eligible && !c.healthy(current)) {
		return false
	}
	readAccount, readOrg := claudeResetIdentity(t.auth)
	account, org := claudeResetIdentity(current)
	return account == readAccount && org == readOrg
}

func findGrant(obs claudereset.Observation, id string) (claudereset.Grant, bool) {
	for _, g := range obs.Grants {
		if g.ID == id {
			return g, true
		}
	}
	return claudereset.Grant{}, false
}

func sameClaim(a *claudereset.Claim, b claudereset.Claim) bool {
	return a != nil && a.AccountID == b.AccountID && a.GrantID == b.GrantID && a.Trigger == b.Trigger
}

func baseline(clears bool, w claudereset.Window) *claudereset.WindowBaseline {
	if !clears || !w.Known {
		return nil
	}
	return &claudereset.WindowBaseline{Used: w.Used, ResetAt: w.ResetAt}
}

// claim re-reads the target with a verified token, repeats the decision on
// that read and submits only if it still holds.
func (c *claudeResetController) claim(ctx context.Context, planned claudereset.Claim) {
	fields := log.Fields{"auth": planned.AccountID, "grant": planned.GrantID, "trigger": planned.Trigger}
	t, ok := c.freshTarget(ctx, planned.AccountID, "", "")
	if !ok {
		return
	}
	verified := c.evaluate(c.claudeAuths(), "")
	if !sameClaim(verified.Claim, planned) {
		return
	}
	grant, ok := findGrant(t.obs, planned.GrantID)
	if !ok {
		return
	}
	account, org := claudeResetIdentity(t.auth)
	now := c.now()
	op := claudereset.Operation{
		ID: uuid.NewString(), RequestID: uuid.NewString(), CredentialID: t.auth.ID, AccountUUID: account, OrganizationUUID: org,
		GrantID: grant.ID, GrantLabel: grant.Label, Class: verified.Claim.Class, ClearsWeekly: grant.ClearsWeek, ClearsFable: grant.ClearsFable,
		Trigger: verified.Claim.Trigger, Reason: verified.Claim.Reason, ResetsLeftBefore: grant.ResetsLeft, GrantEndsAt: grant.EndsAt,
		WeeklyBefore: baseline(grant.ClearsWeek, t.obs.Weekly), FableBefore: baseline(grant.ClearsFable, t.obs.Fable),
		State: claudereset.OpPending, CreatedAt: now, UpdatedAt: now,
	}
	log.WithFields(fields).WithField("reason", op.Reason).Info("claude reset: claiming reset")
	c.submit(ctx, t, op)
}

// submit sends op's claim and settles it. The attempt is journaled before the
// request leaves, so a crash never hides a sent claim, and a resend reuses the
// request ID, which the provider treats as the same claim. After journaling it
// re-checks the gate, the credential, the inventory age and the resend window,
// and the request is bounded by that window. New selections of the account
// wait while the claim and its local recovery run.
func (c *claudeResetController) submit(ctx context.Context, t claimTarget, op claudereset.Operation) {
	fields := log.Fields{"auth": op.CredentialID, "grant": op.GrantID, "trigger": op.Trigger, "operation": op.ID}
	release, errReserve := c.m.AcquirePolicyReset(ctx, op.CredentialID)
	if errReserve != nil {
		return
	}
	defer release()
	op.Attempts++
	op.UpdatedAt = c.now()
	if errSave := c.saveOp(op); errSave != nil {
		log.WithFields(fields).WithError(errSave).Error("claude reset: cannot journal claim; not sent")
		return
	}
	deadline := op.CreatedAt.Add(claudereset.ClaimRetryWindow)
	if !c.gateOpen() || !c.stillTarget(t) || !c.now().Before(deadline) || c.now().Sub(t.obs.ObservedAt) > claudeResetClaimAge {
		if op.Attempts == 1 {
			op.State, op.Result, op.UpdatedAt = claudereset.OpFailed, claudereset.ResultNotSent, c.now()
			_ = c.saveOp(op)
		}
		return
	}
	// Sampled after the checks, so the request never outlives the window.
	timeout := claudeResetCallTimeout
	if left := deadline.Sub(c.now()); left < timeout {
		timeout = left
	}
	if timeout <= 0 {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	result, err := c.client.Claim(callCtx, t.auth, t.token, op.OrganizationUUID, op.GrantID, op.RequestID)
	cancel()
	if err != nil {
		log.WithFields(fields).Warn("claude reset: claim outcome unknown; it will be retried with the same request id")
		return
	}
	op.ConfirmedAt = c.now()
	switch result {
	case "reset", "already_used":
		op.State, op.Result = claudereset.OpDone, result
	default:
		if op.Attempts > 1 {
			// A refused resend says nothing about an earlier attempt whose
			// outcome was lost: that one may have landed while the inventory
			// still shows the old count. Only fewer resets left settles it now;
			// otherwise the claim stays pending (quarantined and counted against
			// the cap) until the post-window reconciliation decides.
			obs, ok := c.readStatus(ctx, t.auth, op.AccountUUID, op.OrganizationUUID)
			if ok {
				if consumed, known := settled(op, obs, c.now()); known && consumed {
					op.State, op.Result = claudereset.OpDone, claudereset.ResultReconciled
					break
				}
			}
			log.WithFields(fields).WithField("result", result).Info("claude reset: resend refused; claim stays pending until reconciliation")
			return
		}
		op.State, op.Result = claudereset.OpFailed, result
	}
	op.UpdatedAt = c.now()
	if errSave := c.saveOp(op); errSave != nil {
		log.WithFields(fields).WithError(errSave).Error("claude reset: cannot journal claim outcome")
		return
	}
	if op.State == claudereset.OpDone {
		log.WithFields(fields).WithField("result", op.Result).Info("claude reset: reset redeemed")
		c.recover(ctx, op, false)
		return
	}
	log.WithFields(fields).WithField("result", op.Result).Warn("claude reset: claim refused; account paused for automatic resets")
}

// settled decides op from a verified inventory read of the same identity:
// fewer resets left means spent, the same count means not spent. A vanished
// grant counts as spent only when a cleared window is reported, fell at least
// claudeResetDropPoints below its own baseline and has not renewed naturally.
// known is false when the evidence cannot decide.
func settled(op claudereset.Operation, obs claudereset.Observation, now time.Time) (consumed bool, known bool) {
	if g, ok := findGrant(obs, op.GrantID); ok {
		return g.ResetsLeft < op.ResetsLeftBefore, true
	}
	for _, item := range []struct {
		before *claudereset.WindowBaseline
		after  claudereset.Window
	}{{op.WeeklyBefore, obs.Weekly}, {op.FableBefore, obs.Fable}} {
		if item.before == nil || !item.after.Known || item.before.ResetAt.IsZero() || !now.Before(item.before.ResetAt) {
			continue
		}
		if item.after.Used <= item.before.Used-claudeResetDropPoints {
			return true, true
		}
	}
	return false, false
}

// recoverPending resolves unresolved claims. Each starts from a fresh read
// verified for the journaled identity: a spent reset is settled at once. An
// unspent one is resent with the same request ID only within the retry window
// and only if the decision still selects the same grant. Later the inventory
// settles it, and a claim that still cannot be decided after UnresolvedAfter
// ends its account's quarantine as unresolved, still counted as spent.
func (c *claudeResetController) recoverPending(ctx context.Context) {
	for _, op := range c.operations() {
		if op.State != claudereset.OpPending || ctx.Err() != nil {
			continue
		}
		now := c.now()
		age := now.Sub(op.CreatedAt)
		if age < claudereset.ClaimRetryWindow && now.Sub(op.UpdatedAt) < claudeResetRetryEvery {
			continue
		}
		if t, ok := c.reconcileTarget(ctx, op.CredentialID, op.AccountUUID, op.OrganizationUUID); ok {
			// Judge renewal at the read's completion, not before it.
			consumed, known := settled(op, t.obs, c.now())
			switch {
			case known && consumed:
				c.settle(ctx, op, claudereset.OpDone, claudereset.ResultReconciled)
				continue
			case known && age >= claudereset.ClaimRetryWindow:
				c.settle(ctx, op, claudereset.OpFailed, claudereset.ResultNotConsumed)
				continue
			case known:
				replanned := c.evaluate(c.claudeAuths(), op.ID)
				if replanned.Claim != nil && replanned.Claim.AccountID == op.CredentialID && replanned.Claim.GrantID == op.GrantID {
					c.submit(ctx, t, op)
				}
				continue
			}
		}
		if age < claudereset.UnresolvedAfter {
			continue
		}
		result := claudereset.ResultUnresolved
		if current, okCurrent := c.m.GetByID(op.CredentialID); !okCurrent || !isClaudeAuth(current) {
			result = claudereset.ResultAccountGone
		} else if account, org := claudeResetIdentity(current); account != op.AccountUUID || org != op.OrganizationUUID {
			result = claudereset.ResultIdentityChanged
		}
		c.settle(ctx, op, claudereset.OpFailed, result)
	}
}

func (c *claudeResetController) settle(ctx context.Context, op claudereset.Operation, state, result string) {
	op.State, op.Result, op.UpdatedAt = state, result, c.now()
	if state == claudereset.OpDone {
		op.ConfirmedAt = op.UpdatedAt
	}
	if errSave := c.saveOp(op); errSave != nil {
		return
	}
	log.WithFields(log.Fields{"auth": op.CredentialID, "grant": op.GrantID, "operation": op.ID, "result": op.Result}).Info("claude reset: unresolved claim settled")
	if op.State == claudereset.OpDone {
		c.recover(ctx, op, true)
	}
}

// recoverLocal finishes local recovery of spent resets, for example after a
// restart between the claim and its recovery, until it succeeds or the cleared
// windows would have renewed anyway.
func (c *claudeResetController) recoverLocal(ctx context.Context) {
	now := c.now()
	for _, op := range c.operations() {
		if op.State == claudereset.OpDone && !op.Recovered && now.Before(claudereset.RecoveryUntil(op)) && ctx.Err() == nil {
			c.recover(ctx, op, true)
		}
	}
}

// restored reports affirmative evidence that a reset took effect on a cleared
// window: its usage fell clearly below the pre-claim baseline. A reading that
// is merely below 100% may still be the old usage. Without a baseline it falls
// back to the window being open.
func restored(before *claudereset.WindowBaseline, obs claudereset.Observation, name string, w claudereset.Window) bool {
	if windowOut(obs, name, w) {
		return false
	}
	if before == nil || before.Used < claudereset.TargetMinUsed {
		// The window was not exhausted at claim time, so an unchanged reading
		// cannot be mistaken for spendable allowance: open is enough.
		return true
	}
	if !w.Known {
		return false
	}
	drop := math.Min(claudeResetDropPoints, before.Used/2)
	return w.Used <= before.Used-drop && w.Used < before.Used
}

// recover makes routing use the restored allowance at once and records it
// only after the recovery is durable. It first takes a verified inventory read
// of the journaled identity and finishes only once that read shows the cleared
// windows open; cooldowns are cleared on that evidence. reserve holds new
// selections of the account during the recovery when the caller does not.
func (c *claudeResetController) recover(ctx context.Context, op claudereset.Operation, reserve bool) {
	fields := log.Fields{"auth": op.CredentialID, "grant": op.GrantID, "operation": op.ID}
	if reserve {
		release, errReserve := c.m.AcquirePolicyReset(ctx, op.CredentialID)
		if errReserve != nil {
			return
		}
		defer release()
	}
	a, ok := c.m.GetByID(op.CredentialID)
	if !ok {
		return
	}
	c.mu.Lock()
	progress := c.recoveries[op.ID]
	if progress == nil {
		progress = &claudeRecoveryProgress{}
		c.recoveries[op.ID] = progress
	}
	fenced := progress.fenced
	c.mu.Unlock()
	// Results of executions dispatched before now predate the reset; fence
	// them once, before the first read, so none lands as a cooldown while it
	// runs. Later passes keep that boundary, so requests dispatched after the
	// reset keep their own evidence.
	if !fenced {
		// Fence every model: a stale credential-scoped refusal must not re-apply
		// a repaired credential-wide cooldown, even after a Fable-only reset.
		if !c.m.fenceClaudeRecovery(op.CredentialID, op.AccountUUID, op.OrganizationUUID, true) {
			return
		}
		c.mu.Lock()
		progress.fenced = true
		c.mu.Unlock()
	}
	obs, ok := c.readStatus(ctx, a, op.AccountUUID, op.OrganizationUUID)
	if !ok {
		return
	}
	weeklyOpen := !windowOut(obs, claudereset.WindowWeekly, obs.Weekly)
	fableOpen := !windowOut(obs, claudereset.WindowFable, obs.Fable)
	if op.ClearsWeekly {
		weeklyOpen = restored(op.WeeklyBefore, obs, claudereset.WindowWeekly, obs.Weekly)
	}
	if op.ClearsFable {
		fableOpen = restored(op.FableBefore, obs, claudereset.WindowFable, obs.Fable)
	}
	// Each cleared window recovers on its own evidence: a confirmed weekly
	// restore clears weekly cooldowns even while Fable is still unconfirmed.
	// The operation is recovered only when every cleared window is confirmed.
	complete := (!op.ClearsWeekly || weeklyOpen) && (!op.ClearsFable || fableOpen)
	if (!op.ClearsWeekly || !weeklyOpen) && (!op.ClearsFable || !fableOpen) {
		log.WithFields(fields).Debug("claude reset: spent reset not yet visible in inventory; recovery retried")
		return
	}
	confirmed := op.ConfirmedAt
	if confirmed.IsZero() {
		confirmed = op.UpdatedAt
	}
	r := ClaudeResetRecovery{CredentialID: op.CredentialID, AccountUUID: op.AccountUUID, OrganizationUUID: op.OrganizationUUID,
		ClearsWeekly: op.ClearsWeekly, ClearsFable: op.ClearsFable, ConfirmedAt: confirmed,
		WeeklyOpen: weeklyOpen, FableOpen: fableOpen, ObservedAt: obs.ObservedAt,
		SessionFull: obs.Session.Known && obs.Session.Used >= 100, SessionResetAt: obs.Session.ResetAt}
	if op.WeeklyBefore != nil {
		r.WeeklyResetAt = op.WeeklyBefore.ResetAt
	}
	if op.FableBefore != nil {
		r.FableResetAt = op.FableBefore.ResetAt
	}
	// Each window's routing recovery runs once; a later pass recovers only the
	// newly confirmed window and keeps newer observations.
	// Fable models become usable once both windows they draw are confirmed:
	// that pass checks both cleared deadlines for them.
	weeklyOK := !op.ClearsWeekly || weeklyOpen
	fableOK := !op.ClearsFable || fableOpen
	c.mu.Lock()
	newWeekly := op.ClearsWeekly && weeklyOpen && !progress.weekly
	newFableModels := weeklyOK && fableOK && !progress.fableModels
	c.mu.Unlock()
	if newWeekly || newFableModels {
		r.Pass, r.Fenced = &ClaudeResetPass{Weekly: newWeekly, Fable: newFableModels}, true
		if err := c.m.RecoverClaudeReset(ctx, r); err != nil {
			log.WithFields(fields).WithError(err).Warn("claude reset: reset spent but local recovery failed; it will be retried")
			return
		}
		c.mu.Lock()
		progress.weekly = progress.weekly || newWeekly
		progress.fableModels = progress.fableModels || newFableModels
		c.mu.Unlock()
	}
	if !complete {
		log.WithFields(fields).Debug("claude reset: reset partly visible; remaining windows retried")
		return
	}
	op.Recovered = true
	if errSave := c.saveOp(op); errSave != nil {
		log.WithFields(fields).WithError(errSave).Warn("claude reset: cannot journal recovery")
		return
	}
	c.mu.Lock()
	delete(c.recoveries, op.ID)
	c.mu.Unlock()
}

// ClaudeResetGrantStatus is one grant in ClaudeResetStatus.
type ClaudeResetGrantStatus struct {
	claudereset.Grant
	Usable         bool   `json:"usable"`
	UnusableReason string `json:"unusable_reason,omitempty"`
}

// ClaudeResetAccountStatus is one account in ClaudeResetStatus.
type ClaudeResetAccountStatus struct {
	ObservedAt       *time.Time               `json:"observed_at,omitempty"`
	Fresh            bool                     `json:"fresh"`
	Healthy          bool                     `json:"healthy"`
	Pending          bool                     `json:"pending"`
	Error            string                   `json:"error,omitempty"`
	Eligible         bool                     `json:"eligible"`
	IneligibleReason string                   `json:"ineligible_reason,omitempty"`
	AtLimit          bool                     `json:"at_limit"`
	Weekly           claudereset.Window       `json:"weekly"`
	Fable            claudereset.Window       `json:"fable"`
	Session          claudereset.Window       `json:"session"`
	BackoffUntil     *time.Time               `json:"backoff_until,omitempty"`
	Grants           []ClaudeResetGrantStatus `json:"grants"`
}

// ClaudeResetStatus is the controller's read-only state for management views.
type ClaudeResetStatus struct {
	Mode           string                              `json:"mode"`
	Active         bool                                `json:"active"`
	InactiveReason string                              `json:"inactive_reason,omitempty"`
	EvaluatedAt    *time.Time                          `json:"evaluated_at,omitempty"`
	Weekly         claudereset.ScopeStatus             `json:"weekly"`
	Fable          claudereset.ScopeStatus             `json:"fable"`
	Planned        *claudereset.Claim                  `json:"planned,omitempty"`
	Accounts       map[string]ClaudeResetAccountStatus `json:"accounts"`
	Operations     []claudereset.Operation             `json:"operations"`
}

// ClaudeResetStatus reports the automatic Claude reset state, or false when
// the controller is not running. Raw credential IDs key the accounts. It never
// calls the provider.
func (m *Manager) ClaudeResetStatus() (ClaudeResetStatus, bool) {
	if m == nil {
		return ClaudeResetStatus{}, false
	}
	m.claudeResetMu.Lock()
	c := m.claudeResets
	m.claudeResetMu.Unlock()
	if c == nil {
		return ClaudeResetStatus{}, false
	}
	return c.status(), true
}

func (c *claudeResetController) status() ClaudeResetStatus {
	in := c.input(c.claudeAuths(), "", false)
	plan := claudereset.Decide(in)
	now := in.Now
	c.mu.Lock()
	out := ClaudeResetStatus{Mode: "auto", Active: c.allowed && c.journalErr == "", InactiveReason: c.gateReason,
		Weekly: plan.Weekly, Fable: plan.Fable, Planned: plan.Claim, Accounts: map[string]ClaudeResetAccountStatus{}}
	if c.journalErr != "" && out.InactiveReason == "" {
		out.InactiveReason = "journal_unavailable"
	}
	if c.gateReason == "off" {
		out.Mode = "off"
	}
	at := now
	out.EvaluatedAt = &at
	errs := map[string]string{}
	for id, e := range c.statuses {
		errs[id] = e.err
	}
	c.mu.Unlock()
	for _, a := range in.Accounts {
		st := ClaudeResetAccountStatus{Healthy: a.Healthy, Pending: a.Pending, Error: errs[a.ID], Eligible: a.Eligible, IneligibleReason: a.IneligibleReason,
			AtLimit: a.AtLimit, Weekly: a.Weekly, Fable: a.Fable, Session: a.Session, Grants: []ClaudeResetGrantStatus{}}
		if !a.ObservedAt.IsZero() {
			observed := a.ObservedAt
			st.ObservedAt = &observed
		}
		st.Fresh = !a.Weekly.ObservedAt.IsZero() && now.Sub(a.Weekly.ObservedAt) <= claudereset.StatusFreshFor
		if a.BackoffUntil.After(now) {
			until := a.BackoffUntil
			st.BackoffUntil = &until
		}
		for _, g := range a.Grants {
			usable, reason := claudereset.Usable(a, g, now)
			st.Grants = append(st.Grants, ClaudeResetGrantStatus{Grant: g, Usable: usable, UnusableReason: reason})
		}
		out.Accounts[a.ID] = st
	}
	ops := c.operations()
	if len(ops) > 50 {
		ops = ops[:50]
	}
	out.Operations = make([]claudereset.Operation, 0, len(ops))
	for _, op := range ops {
		op.AccountUUID, op.OrganizationUUID, op.RequestID = "", "", ""
		out.Operations = append(out.Operations, op)
	}
	return out
}
