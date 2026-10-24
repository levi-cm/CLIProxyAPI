# Renewal-priority routing candidate

Production integration is deferred. This candidate starts at `2896a388` in
branch `feat/renewal-priority-routing`, isolated from the production checkout,
configuration, credentials, journals and durable conversation bindings.

## Priority and evidence

For new unbound work, prefer the exact earliest eligible deadline, including the
existing mandatory expiring-credit guard. There is no near-equal deadline band,
percentage target or account-consumption estimate in normal routing.

When fresh evaluation is unavailable, retain a previously observed, valid future
ordinary weekly renewal as ordering evidence only. A stale measurement cannot
prove allowance availability, consumption rate or a future workload forecast.
Existing hard exclusions remain authoritative. Prefer accounts with a future
observed deadline over accounts with no usable deadline; use the configured
fallback only when none has one. Never infer a new cycle from a passed renewal.
Expired/stale reset inventory does not grant redemption authority.

## Automatic spreading safeguards

Automatic spreading is considered for new, unbound work only. Every selection
rechecks all of the following; dashboard polling does not control routing:

1. Exactly two eligible Codex accounts have distinct logical identities. Both
   ordinary weekly renewals are strictly more than 96 hours away and no more
   than four hours apart. The exact earlier deadline wins normal selection;
   proximity alone never enables distribution.
2. Both accounts have fresh quota observations and complete fresh reset
   inventory within the configured freshness window (at most 120 seconds).
   Current settings and the newly fetched snapshots are reevaluated before
   forecasting: holds, reserves, force selection, authentication blocks,
   exhaustion, model eligibility and actionable expiring-credit guards remain
   authoritative. An earlier saved-credit guard prevents automatic spreading.
3. Each account has one unambiguous applicable ordinary weekly quota window.
   Forecasts use actual provider-reported percentage changes for the earlier
   account, not request counts, token totals, or percentages from the later one.
4. A private in-memory history must accumulate thirteen anchors at least five
   minutes apart spanning at least one hour. Gaps exceeding ten minutes,
   identity/registration/renewal changes, decreasing usage (refill/correction),
   stale evidence or invalid time gates invalidate forecasts. Restart warms cold.
   No production history journal is read or written to construct this cache.
5. Let `r` be the slowest observed consumption rate (percentage points/hour)
   over these intervals, also including any newer partial interval. The forecast
   is accepted only if `r > 0` and
   `(r / 4) * (hours_until_renewal - 24) >= 2 * remaining_percent`.
   This discounts the measured rate by 75%, excludes the final 24 hours, and
   requires twice the remaining allowance in projected consumption. A zero or
   insufficient recent burn interval immediately vetoes spreading.
6. Even with that forecast, the later account receives work only when the
   earlier account has at least four concurrent active/reserved attempts and
   at least two more than the later account. Comparison and reservation share
   one runtime lock across callers/models. Ties and smaller gaps favor the
   earlier account. Live completion removes pressure immediately, with no
   allocation target, request-count debt or later-account catch-up.

A failed gate immediately restores normal ranking for subsequent selections;
active streams and bound owners remain on their original accounts. Stale quota
or inventory invalidates the accumulated forecast and requires new warmup.
Forecast decisions are explicitly recorded as
`automatic_concurrent_pressure_forecast`. Rates can change, demand can cease,
and concurrent attempts can have very different costs. These safeguards reduce
forecast risk; they do not guarantee that the earlier allowance will be consumed
or that spreading has zero risk of unused allowance.

## Manual spreading

The existing revisioned, explicitly enabled, cancellable override gains mode
`pressure`. Its two `accounts` contain credential IDs and no nonzero `percent`.
It uses the same durable override identity validation, absolute expiry,
1–24-hour limit, exhaustion termination and safety controls. The dashboard starts
at one hour. Single-account overrides retain their existing behavior. Legacy
`split`
requests and persisted state retain their participants, expiry, cancellation and
identity checks, but their percentage fields no longer allocate requests: they
use the same concurrent-pressure algorithm as `pressure`. API capabilities
report `percentage_split: false` and `legacy_split_membership: true`.

Pressure spreading is restricted to the two selected eligible accounts. Order
those accounts by exact renewal deadline. On every new selection, compare live
active attempts plus reservations for selections that have not entered execution.
Use the earlier account unless it has at least two more attempts than the later
account. The four-active-attempt and forecast gates apply to automatic spreading;
manual spreading is an explicit operator exception. Reserve the selected account
atomically with the count comparison, across
models and callers. Completion releases its count; retries transfer/release
counts through the existing lease lifecycle. No cumulative request target or
catch-up debt exists. A running stream holds its lease through completion.

Unknown/stale deadline evidence or missing request-lease accounting prevents
pressure spreading and retains normal deadline selection. A credential reserved
for a reset cannot receive a pressure reservation. Hard eligibility and model
support are evaluated before spreading. Expiry/cancellation is checked when
selecting, without depending on dashboard polling. Bound conversations keep their
owner during manual and automatic spreading,
including idle self-contained turns. Existing safe failover and deadline-driven
migration outside manual spreading remain intact.

Concurrency counts measure occupied attempts, not request cost or allowance.
They cannot identify a compute-heavy request or promise equal resource pressure.
Manual spreading intentionally permits later-account consumption and may leave
earlier allowance unused. It is an operator decision, not a forecast or a
zero-risk guarantee. Normal deadline preference guarantees selection ordering
under its observed evidence and eligibility constraints; it cannot guarantee
that enough future demand exists to consume an account's allowance.

## API

Both maintained management and inference-authenticated routing APIs accept:

```json
{
  "mode": "pressure",
  "accounts": [{"credential_id": "a"}, {"credential_id": "b"}],
  "duration_hours": 1,
  "expected_revision": 0
}
```

No API under `/v0/management` changes. Cancellation, revision conflicts, history,
mandatory reset automation and account ownership checks use existing machinery.

## Production gate

No merge, restart, recreation, listener change, live config edit or second writer
against production state is part of this task. Integration requires an approved
quiet window and a rehearsed zero-downtime handoff: keep loopback, LAN and
Tailscale access; drain existing HTTP streams and WebSockets on the old process;
retain conversation/account ownership and journals; maintain mandatory automatic
refills under exactly one authoritative writer; and verify rollback without
splitting state ownership. A quiet window alone does not waive zero downtime.
If the deployment cannot support these requirements, production remains running.

## Verification

Verified on 2026-10-24 with Go 1.26.0, Node 22 and Chromium. All provider
requests, resets, credentials and policy state in tests were isolated fixtures.

- Full uncached Go baseline and final suite passed:
  `GOMAXPROCS=4 go test -p 1 ./... -count=1`.
  Final log: `/tmp/cliproxy-renewal-final-full.log`.
- Affected race checks passed:
  `GOMAXPROCS=4 go test -race -p 1 ./internal/accountpolicy
  ./internal/accountpolicyapi ./internal/accountpolicyui ./sdk/cliproxy/auth
  ./sdk/cliproxy ./internal/api ./test -count=1`.
  Log: `/tmp/cliproxy-renewal-final-race.log`.
- Required `go build -o test-output ./cmd/server` passed; binary removed.
  Focused `go vet`, `gofmt` and `git diff --check` passed.
- All 38 JavaScript tests and asset/script syntax checks passed. Log:
  `/tmp/cliproxy-renewal-final-js.log`.
- All five fixture browser suites passed: history/manual routing, 100-account
  dashboard, observation-only behavior, weekly timeline, and mandatory reset
  controls. History verified one-hour default, pressure apply/cancel, retained
  drafts, risk text and desktop/mobile layout. Browser logs are
  `/tmp/cliproxy-renewal-final-browser-*.log`; the final timeline pass is
  `/tmp/cliproxy-renewal-final-browser-weekly-rerun.log`.
- The existing timeline script initially failed because fixed October credit
  dates and a whole-axis date exclusion no longer matched the live preview
  date. The test now supplies relative fixture-only expiry dates and checks
  actual credit-marker identities. Product timeline behavior was unchanged.
- Eight authenticated accessibility snapshots passed at 100% (four views in
  light/dark themes). These are automated accessibility checks, not an
  exhaustive accessibility guarantee.
- Independent code review and focused race tests passed. Review found and
  verified the refreshed-snapshot eligibility race; regressions reproduced it
  before the fix and passed after current evidence was reevaluated.

Deterministic regressions cover nanosecond renewal differences, strictly >96h,
<=4h, burst reservations, no/low/uneven provider burn, stale quota and inventory,
refills, identity/renewal changes, partial-interval slowdown, cold warmup,
expiring-credit priority, bound/encrypted ownership, reset exclusion, model
support, override expiry/cancellation/restart, and 429 failover with a preserved
one-hour Retry-After. No wall-clock sleeping is used for routing time boundaries.

Production remained container `94eef1aafb04`, image
`cliproxyapi-fork:allowance-2896a388`, restart count zero. Read-only `/healthz`
checks returned 200 on loopback, LAN and Tailscale. The production checkout's
preexisting dirty and untracked files were unchanged. These health checks do
not claim a deployment, connection-draining rehearsal, or continuous telemetry
of every active stream; production integration remains deferred.
