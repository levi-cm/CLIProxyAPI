# Optional account policy

This module owns provider observations, deadline evaluation, reset-credit
selection and the durable redemption state machine. It imports neither the
proxy config package nor SDK auth/API code. Integration adapters are separate
`account_policy_*.go` files in the SDK and API; see
[the integration map and update procedure](../../docs/account-policy-upstream-updates.md).

## Immediate exhaustion rule

With policy enabled and `automation: auto_expiring`, fresh ordinary **weekly**
exhaustion immediately selects the earliest eligible credit whose expiry is
strictly before the next normal weekly refresh. It requires neither queued work
nor proximity to expiry. A credit with 11 hours remaining is spent now if weekly
allowance is exhausted and normal renewal occurs after that expiry. Credits
expiring at or after normal renewal stay saved. Short-window denial alone does
not establish weekly exhaustion; provider `allowed` is an aggregate scope flag,
not evidence that both windows are exhausted. Explicit permission also prevents
interpreting a rounded displayed 100% as exhaustion.

The expiry-minus-guard safety deadline remains an independent fallback when
covered allowance has already been used but is not exhausted. Nonexpiring
credits are excluded from `auto_expiring`; `auto_all_saved` is a separate explicit
authorization with demand, complete-inventory and saved-credit reserve rules.
`off`/`notify` never automatically redeem. An explicit operator schedule or
selected manual redemption is distinct from automatic mode.

All writes still require verified account/workspace ownership, supported type
and scope, unexpired known credit details, fresh usage and inventory, durable
journal availability, and an idle account reservation. Holds stop automatic
redemption; read-only mode stops provider writes. Unknown expiry, purchased
balances and unsupported classes never become guessed expiring resets.

## Success and recovery

Journal the selected credit and request UUID before submission. Uncertain
responses retain that UUID; read-only discovery can reconcile a consumed credit
and recovered allowance without submitting again. Distinguish `reset`,
`already_redeemed`, `nothing_to_reset` and `no_credit`. Repeated unchanged evidence
must not create new logical operations. Schedules retain their original upstream
owner so credential re-registration cannot redirect an approved write.

After redemption, reread usage and selected inventory, verify allowance recovery
and credit consumption, and publish the new weekly refresh time. The SDK recovery
adapter clears only affected older quota cooldowns and persists repairs before
publishing. Newer failures, authentication blocks and unrelated model cooldowns
survive. Routing evaluates the refreshed snapshot on the next selection; it does
not splice an in-flight stream or move an unsafe conversation continuation.

Use fake clocks in tests. Relevant passive quota events request a coalesced fresh
provider observation; successful requests that merely advance SDK result
generation must not trigger per-request discovery polling. Native lifecycle
continues independently of the browser. No live provider writes are needed to
run the regression suite.

## State and rollout

Use one protected local state directory and writer instance. Journal locking
fails closed on unsupported platforms. Disable the policy and automation in YAML
for rollback, retaining credentials and pending operation state. YAML disabled/
off is the restart authorization ceiling for persisted operator settings.
Malformed contract/identity failures inhibit automatic writes while preserving
uncertain operations for read reconciliation; repair the provider contract and
review pending state before explicitly restoring write authority.

Deployment, tailnet/operator boundaries and staged activation are documented in
[the deployment guide](../../docs/account-policy-deployment.md). Production
services and remote-device ACL verification are operator provisioning tasks, not
claims made by the fixture tests.

## Credit balance spending

`credit-spending` is separate from saved reset credits. The Codex usage response
reports an account credit balance (`credits.balance`, a decimal string). Upstream
draws that balance only after the account's included allowance is exhausted;
there is no request option to spend credits earlier.

When enabled, an account whose ordinary allowance is exhausted stays eligible
while its balance is fresh (`max-balance-age-seconds`) and above
`floor-credits + margin-credits`. Its routing deadline becomes
`now + allowance-guard-hours`, so it ranks before allowance that renews later
(credit-first) and after allowance that renews sooner and would otherwise be
lost. Holds, forced accounts, authentication or identity failures, spend
controls, model-specific limits and stale or unknown balances never use credits.

The floor is never spent automatically. A per-account
`credit-reserve-unlocked-until` (at most 24 hours ahead) temporarily lowers the
floor to zero; the margin still applies. `credit-value-usd` and `usd-to-eur` only
drive estimated value displays. Burn rates come from the last two hours of
in-memory balance observations and reset on restart. Nothing here purchases,
reloads or redeems anything. Zero numeric values select the defaults, so a zero
floor or guard cannot be configured; use the manual unlock instead.

Every credit-serving account ranks at `now + allowance-guard-hours`. Credits are
therefore spent before any allowance renewing later than that, however much of it
remains; allowance renewing inside the window is used first. Under
`auto_all_saved`, saved non-expiring resets are not redeemed early while the
account is serving on credits; they keep their previous timing once the floor is
reached. Expiring resets are unaffected.

The floor is a routing threshold, not a provider limit. Streams and account-bound
continuations already on the account may finish after a later poll shows the
floor reached; the margin absorbs this. If upstream answers a credit-served
request with `usage_limit_reached` (429), the existing credential cooldown applies
until the provider's reset time and other accounts take over. A credit top-up
does not clear that cooldown.

Journals written before this feature adopt `credit-spending` from the startup
configuration once. Afterwards the journal is authoritative, like other operator
settings; change it through the dashboard or management API, or a configuration
hot reload.

## Claude weekly deadlines and focus mode

Claude subscription accounts have no policy snapshot. Their weekly reset and
utilization come passively from the `Anthropic-Ratelimit-Unified-7d-*` response
headers each account returns. New Claude conversations go to the account whose
weekly allowance resets first. Until every eligible Claude account has been
observed (for example after a restart), the configured fallback selects instead,
so unobserved accounts are not starved. A reset that has passed is projected
forward by whole weeks for ordering only.

Accounts that routing keeps idle would otherwise go stale. Every five minutes,
each enabled Claude account whose last observation is at least four minutes
old reads `GET https://api.anthropic.com/api/oauth/usage` (the read-only
endpoint behind Claude Code's `/usage`; no model request, no allowance used).
The result is stored as the same unified headers, so usage dashboards and the
deadline evidence stay fresh, and the evidence exists shortly after a restart.
A failed read keeps the previous observation, and a response observed after the
read started is never overwritten. Only the passive observation changes.

Running conversations normally keep their account. Focus mode moves them only
when the earliest-resetting account resets within 48 hours and, at its own
utilization pace over the trailing 24 hours (at least one hour of observations,
measured up to now), would otherwise reach its reset with at least 10 percentage
points unused. Focus stays on while any leftover is still projected. A move
happens at the conversation's next turn and costs one prompt-cache rebuild on
the focus account. Only model families whose thinking stays valid across
accounts move (Opus, Fable, Mythos). Sonnet 5.5 and Haiku 5.5 thinking is
account-bound and would be dropped, so those conversations stay. Requests that
reference workspace-scoped state (Files API sources, code-execution containers,
server tool results) also stay. Set `claude_focus` to `off` through the settings
API to disable moves. Transitions are logged as `claude focus changed`, and each
move is recorded as decision `claude_focus_migration_from_<credential>`.

A replayable Claude turn whose bound account became unavailable moves without
waiting for that account to become idle, because Claude requests carry their
full history and no account-local continuation state. The bound credential's
provider decides this, not the route namespace: the manager routes every
request under `mixed`. A turn that references workspace-scoped state (Files API
sources, code-execution containers, server tool results) cannot succeed on
another account and keeps its 409. Sonnet 5.5 and Haiku 5.5 turns do move:
another account drops their earlier thinking instead of refusing the request.
Codex keeps its stricter rule.

### 5-hour and Fable windows

Two narrower windows can refuse a Claude account before its weekly allowance
is used up. The 5-hour session window (`Anthropic-Ratelimit-Unified-5h-*`)
refuses every model. The Fable-scoped weekly window (`...-7d_oi-*`) refuses
Fable only, and Anthropic reports it on Fable responses only. The selector keeps
the last reported state of both windows per account. It learns them from every
response, including the 429 refusal, and from the usage poller (the `session`
limit and the `weekly_scoped` limit for model "Fable").

A window is full when its status is `rejected` or its utilization reaches 1.0.
Results are observed when they complete, so a long stream can deliver headers
older than another result's. Because resets only move forward and utilization
never falls within one window, an observation of an earlier window, or an open
reading of a window already seen full, is ignored. Results for accounts the
manager no longer holds are ignored.
Until its reset passes, a full 5-hour window takes the account out of Claude
routing, and a full Fable window takes it out of Fable routing only, so Opus
keeps using the focus account. New conversations then go to the earliest weekly
reset among accounts with an open window (an account without a 5-hour window
is always open), and bound conversations move as above. A full window without
a known reset, or every account full at once, leaves the decision to upstream
and the reactive cooldown. Transitions are logged as `claude limit window full`
and `claude limit window open`.

## Claude reset grants

Claude Max accounts hold reset grants in Anthropic's `cedar_ember` program: a
monthly full reset that clears the weekly and the Fable weekly window, and Fable
resets that clear only the Fable window. `sdk/cliproxy/auth/claude_reset_controller.go`
spends them automatically. The decision rules live in `internal/claudereset`.
Anthropic lists the program only to Claude Code clients, so these calls identify
as the Claude CLI.

A grant adds value only when the fleet would otherwise refuse demand.
Allowance that renews unused is lost, and spending a grant early only moves
consumption onto the restored allowance. A grant is therefore spent in three
cases:

- **Weekly need.** The open weekly allowance across healthy, unheld accounts is
  below 5 points (100 points is one account's week), and a refused request
  needed it within the last 15 minutes. Need is a request on a Claude route
  that finally failed with a refusal (including one delivered in a stream),
  and either:
  - one of its attempts was refused with rate-limit headers marking the weekly
    window `rejected` (attributed by the attempt's resolved model, so alias
    routes count); an attempt refused while another account served the
    retry does not count; or
  - it was refused locally because no credential could serve (all cooling
    down, possibly beside logged-out ones, or no account the policy allows,
    such as a forced account on cooldown), while every account the request
    could use when it started was blocked by its weekly window or by a
    verified 5-hour window with a known renewal more than 30 minutes away.
    It is judged at refusal time from fresh verified inventory of the window running now (not renewed since it was read). At
    least one account must be weekly-exhausted: a fleet blocked only by 5-hour
    windows is not need, because no grant reopens a 5-hour window. An
    active cooldown counts only when its deadline cannot also be the Fable
    window's: the two usually renew together, and a Fable refusal can block a
    whole credential while weekly allowance remains. Later fleet changes cannot
    give an earlier refusal a weekly cause.
  Successful traffic, 5-hour, entitlement and request-scoped refusals are not
  need. A refusal wakes the controller at once. A weekly grant
  is spent on the exhausted account (≥ 95% used) whose weekly window renews
  last. That gives the restored allowance the most time to be used, and it
  postpones the least if Anthropic restarts the window on redemption.
- **Fable need.** The same check for the Fable window (`7d_oi`), with a
  refused Fable request. Only Fable grants are spent, on a Fable-exhausted account that has
  weekly allowance left right now, because Fable also draws the weekly window.
- **Expiry.** In the last 6 hours before a grant ends, it is spent if the exact
  windows it clears hold at least 1% usage and none of them renews before the
  grant ends. An expiring grant costs nothing. Inventory reads speed up 24
  hours ahead.

**Safety rules:**
- While routing is forced to one credential (`force-account`), no other
  account counts as serving.
- Decisions use only inventory read with the credential's current, verified
  token. Response headers and the usage poller only decide when to read early.
- An account whose last claim may have spent a reset counts as restored until
  its recovery confirmed the reset from a verified read; a read right after the
  claim may still show the old usage. One reset never justifies another on
  stale evidence.
- Each window is judged on its own observation time. A window without evidence
  from the last 15 minutes, or renewing within 30 minutes, counts as open, so
  stale data never spends.
- An account with a known-full 5-hour window is not a target for any trigger,
  however soon it renews.
- Need claims that spent or may have spent a reset (pending or unresolved)
  are at least 10 minutes apart and capped at 3 per 24 hours.
- Need is bound to the routing pool its request started under: the accounts
  that could serve then (enabled, not terminally unauthorized, with an
  unexpired access token, as selection requires; not held; not excluded by
  `force-account`), each keyed by credential, account and organization. An
  account that cannot serve adds no allowance and receives no reset. It counts only while
  that pool is unchanged, so a hold or override change cannot hand one pool's
  refusal to accounts that served all along. The pool is computed when the
  request starts, together with a routing revision that advances on every
  hold or force-account change. A refusal counts only if the pool and the
  revision are the same when the request fails and when the claim is decided,
  so a change during the request (which may have caused the refusal), or a
  hold followed by a release, retires it. A need claim, or its resend, is
  sent only if both are still unchanged after it is journaled; a local
  refusal also needs evidence for every account of the starting pool.
- One refusal justifies one reset: a new need claim requires a refusal after
  the last need claim, which may already have met that shortage. A resend of
  a pending claim keeps the refusal that started it.
- A claim on an account needs an inventory read taken after that account's
  previous claim.
- A refused claim pauses that account for one hour.

**The claim itself:**
- **Fence.** Journal writes and claims happen only under the account-policy
  `redemption.lock`, which is the deployment runner's fence, and with the writer
  gate re-checked inside it. Shutdown drains the controller before it cancels
  the run context that keeps writer ownership.
- **Identity.** Inventory is decision-grade only when read with a token whose
  provider profile matches the credential's stored account and organization.
  The claim is sent with that same token, registration and identity, right
  after a fresh read and a repeated decision.
- **Journal.** Each attempt is journaled in `claude-resets.json`, in the policy
  state directory, before it is sent. The journal is reloaded from disk under
  the lock before every decision.
- **Re-checks.** After the attempt is journaled, the gate, the credential, the
  read's age (at most 90 seconds) and the resend window are checked again. The
  request is bounded by that window. A first attempt that fails these checks is
  settled as `not_sent`, without a pause. New selections of the account wait
  while the claim and its local recovery run, and active streams continue.
- **Unknown outcome.** An unresolved claim always starts from a fresh verified
  read of the journaled identity, and fewer resets left settles it as spent. It
  is resent with the same request ID only within 9 minutes, which Anthropic
  treats as idempotent (`already_used`), and only if the decision still selects
  that grant for the same trigger. A refused resend proves nothing about an earlier attempt whose
  outcome was lost, so it leaves the claim pending. After the window, an
  unchanged count settles it as unspent. A vanished
  grant counts as spent only when a cleared window is reported, its usage fell,
  and it has not renewed.
- **Reconciliation reads.** Settling a claim from a verified read ignores
  holds and `force-account`, which only restrict where claims are sent.
- **Quarantine.** An unresolved claim quarantines its logical account (account
  UUID, whatever the credential is called), which counts as restored for need
  decisions. It settles as `unresolved` after one hour and still counts against
  the cap.

**After a spent reset:**
- Recovery first records the execution watermark, so results of executions
  dispatched before then cannot apply cooldowns while it runs. Then it takes a
  verified inventory read.
- Each cleared window recovers on its own evidence. A window that was
  exhausted at claim time needs its usage to fall clearly below the pre-claim
  baseline; any other window only needs to be open.
- The execution fence is taken once per reset, and each routing recovery step
  runs once, so later passes keep evidence from requests dispatched after the
  reset.
- Fable models recover in the pass where every window they draw is first
  confirmed, checking both cleared deadlines.
- The selector forgets the Fable window up to the start of the verified read
  that confirmed the reset. That supersedes a lagging post-claim read that
  still showed the window full with the same reset time, and newer evidence
  stays.
- The operation counts as recovered only once every cleared window is
  confirmed. Until then the account counts as restored for need decisions, and
  no other claim is made on it.
- `RecoverClaudeReset`, modeled on the Codex recovery, clears only quota
  cooldowns that end at a cleared window's renewal time (plus fuzz), whose
  window that read shows open, and that were recorded before the read. The
  credential-wide cooldown is decided by the read alone, because passive reads
  advance its timestamp. A refusal of several windows sets it to the latest
  deadline, so it is repaired by whichever cleared window that deadline
  belongs to, Fable included. The recovery fence covers every model, so a
  stale credential-scoped refusal cannot re-apply it. Model states that the
  refusal wrapped in the same cooldown (`credential_quota`) are released too,
  and an independent cooldown kept below the wrapper is reinstated. A Fable cooldown also needs the weekly window open. A
  deadline that also matches a still-full 5-hour window, or an unknown cause,
  stays.
- It persists the credential and the cooldown store before publishing.
- It records the execution-sequence watermark, so results of earlier
  dispatches cannot re-apply cooldowns or quota headers.
- The selector forgets the Fable window when the grant clears it, for that
  registration only, fenced against older observations. A reset can reopen a
  window without moving its reset time, which the staleness rule would
  otherwise ignore. A manual "clear cooldown" forgets every window the same
  way.
- The recovery is journaled only after it succeeded. Until then it is retried,
  across restarts, until the cleared windows would have renewed anyway. Then a
  fresh usage reading is published.
- The usage poller stamps its readings with when the read began, so a read
  started before a reset cannot pass the reset's fence.

**Reads and claims:**
- Inventory is read hourly, and every 5 minutes while a trigger is near.
- Reads always happen. Claims happen only while this instance holds the
  account-policy owner lock, the policy is enabled and not read-only, and
  `claude_resets` is not `off`.
- Journals written before this setting existed adopt the startup value.
- Isolated deploy fixtures have no network, and the compose dry run has no
  credentials.

`GET /v8/management/account-policy/accounts` and `/diagnostics` report the
state as `claude_resets`: the mode and gate, both need triggers with their
reasons, the planned claim, each account's windows and grants, and recent
operations. Request and organization identifiers are omitted. The status never
calls the provider.
