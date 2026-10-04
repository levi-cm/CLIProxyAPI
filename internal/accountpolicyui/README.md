# Maintained account dashboard

The existing management panel remains at `/management.html`. This package adds
the native optional policy controls at `/account-policy.html`, with embedded,
maintained CSS and JavaScript. Overview shows every currently serving account,
retained usage statistics, measured time graphs and a per-account reset timeline.
Accounts supports search, provider/status filters, sorting and pagination rather
than assuming a two-account pool. Routing & settings separates the main choices
from advanced safety controls and explains every option. History preserves
selections, schedules and durable redemption outcomes. Light, dark and system
themes use the main panel's restrained neutral palette without patching its
downloaded assets.

It obtains snapshots and writes only through the authenticated
`/v8/management/account-policy` APIs. Visible connected pages read the local
`/dashboard` snapshot every five seconds and refresh local quota/history
snapshots every thirty seconds. Polling pauses when hidden or during an operator
action. Countdown ticks never fetch the provider or management API. Only the
explicit Refresh quota action requests immediate provider observations; background
discovery belongs to the independent service, not browser polling. Opening the page
does not enable policy or reset automation. The management key is held in page memory only;
disconnect clears it. The inference API key is separate.

## Evidence and missing data

Current activity comes from the proxy's request counters, including registered
providers without supported quota observations and when policy is disabled.
Multiple accounts can be serving concurrently. Last selected is separate
history, not an active-account claim. Samples older than fifteen seconds become
stale; failed activity reads become unavailable, never falsely idle. Account
identity is stable across sorting and pages. An open account form and unsaved
settings are not replaced by background updates.

Usage comes from the optional bounded, sanitized retained completion sink.
The request total counts usage records (provider attempts); retries and
additional model records can exceed the number of downstream client requests.
Collection starts when routing or the independent **Collect dashboard data**
opt-in is enabled; previous traffic
cannot be reconstructed. Retained observations remain readable when collection
is off. Requests, tokens and latency are proxy measurements, not subscription
allowance. Missing or ambiguous token measurements remain unavailable, not
invented zero-token usage. Graphs show occupied measured buckets only; missing coverage is not
plotted as zero. The timeline distinguishes ordinary weekly/short-window
refreshes, reset expiry and the safety fallback. Out-of-range events retain
their exact timestamps rather than masquerading as endpoint markers. Its
account rows follow the Accounts filters and current page.

`account-policy.observations-enabled: true` enables bounded provider quota/inventory
reads and private completion records while `enabled: false` and `automation: off`
preserve ordinary routing and disable reset execution/cooldown recovery. Both
switches default false. The observations opt-in is config-authoritative on restart;
UI changes apply at runtime, so also set YAML for a persistent opt-in. Idle provider
discovery is approximately five minutes plus stable jitter; active/near-expiry
accounts refresh approximately once a minute, respecting provider backoff.
The two-minute action freshness window is deliberately stricter: the UI keeps
last-known evidence with stale labels, but does not treat it as permission to route
or redeem. Inventory counts require an actual inventory observation. Empty usage
has an explicit waiting/collection-off explanation, not inferred historical zeros.
The upstream Redis usage queue is not read or drained by this dashboard.

The read-only dashboard endpoint accepts `range=1h`, `24h` or `7d`, is protected
by the existing v8 management middleware, and returns allowlisted local fields.
Its activity source and usage sink are injected through small optional SDK/API
hooks. No provider client, inference API, deprecated v0 endpoint or downloaded
management asset needs changing. See `docs/account-policy-upstream-updates.md`.

## Existing panel audit

Read-only audit of `router-for-me/Cli-Proxy-API-Management-Center` at
`ee79a794526a30c03748a8864a9ac6589a31833b` found the owner's existing capability:

- `src/features/quota/providers/codex/CodexQuotaBody.tsx` displays the available
  count and one row per known reset, with absolute times and countdowns.
- `src/features/quota/providers/codex/data.ts` loads usage and detailed inventory
  through management `api-call`, scoped to a credential `authIndex`. It consumes
  a reset with a newly generated `redeem_request_id`, but does not select
  `credit_id`, journal retries, verify response outcome, or validate workspace.
- `src/utils/time/sharedClock.ts` supplies an existing shared minute clock.
  `timezone.ts` correctly derives the offset when given a target date, but the
  reset list calls its label with the current date once for the whole list.
  Therefore a Berlin list spanning October's DST change can label later rows
  GMT+2 despite their local time having GMT+1. Its minute clock does not update
  every second during the final minute.
- Provider cards use credential ownership; three credit rows are not three
  accounts. The policy page maintains provider groups, explicit account and
  workspace ownership, and one row per structured credit. Missing details are
  shown as unavailable details rather than fabricated rows or timestamps.

This checkout has no maintained source of that external React panel; its
`internal/managementasset/updater.go` downloads `management.html`, which automatic
updates can replace. The policy page is the specification's permitted separate
extension. No downloaded panel, existing reset list, or existing polling flow
was changed. Do not open both views and manually refresh the provider repeatedly;
the policy page uses the single native discovery service's snapshots. Upstream
panel consume controls operate independently of the native journal; use this
policy view for selected, durable redemption when the module is enabled.

## Fixtures and verification

```sh
go run ./cmd/account-policy-preview -listen 127.0.0.1:8318
# Open http://127.0.0.1:8318/account-policy.html with management key fixture-key.
node --test internal/accountpolicyui/*test.cjs
go test ./internal/accountpolicyui ./cmd/account-policy-preview
# With Playwright installed (or PLAYWRIGHT_MODULE_PATH pointing at its package):
ACCOUNT_POLICY_PREVIEW_URL=http://127.0.0.1:8318 node internal/accountpolicyui/scripts/verify-dashboard.cjs
# Optional authenticated Lighthouse snapshot audits (Lighthouse + puppeteer-core):
ACCOUNT_POLICY_PREVIEW_URL=http://127.0.0.1:8318 node internal/accountpolicyui/scripts/audit-dashboard.mjs
```

The preview is test-only and never loads config, OAuth material, or any provider
client. Its listener accepts only explicit loopback or Tailscale IPv4 addresses;
wildcard, public IP and DNS listeners are rejected. `-listen 100.82.251.30:8318`
allows an isolated private tailnet fixture check. The page identifies fixtures
after connection. The two main accounts exercise four-day and seven-day weekly
windows, with three detailed credits owned by B. Their UTC expiries reproduce
the reported October 5/22/29 times in fixed GMT+2. Berlin mode instead shows
October 29 as 18:48 GMT+1 without changing its provider instant.

Fixture controls require `Authorization: Bearer fixture-key`:

- `GET /fixture/state` exposes management request counts and current fixtures.
- `POST /fixture/control` with `{"near_expiry":true}` sets B's earliest credit
  45 seconds ahead of the real clock for final-minute browser verification.
- `{"failure":"schema_mismatch"}` injects a one-shot write error.
- `{"reset":true}` restores initial fixture state.
- `{"account_count":100,"active_accounts":["account-a","account-100"]}`
  creates a larger pool with concurrent activity and mixed providers (2–100
  accounts). Includes a very long alias to exercise wrapping.
- `{"stale_activity":true}` makes activity stale without claiming idle.
- `{"dashboard_unavailable":true}` returns a recoverable telemetry failure;
  `false` restores reads. Neither control changes production policy.

The preview also supports successful selected redemption, schedules/cancel,
hold/reserve/settings, local cooldown, sanitized diagnostics, auth failure and
cross-account rejection. Its operations are fixture outcomes, not evidence of
live backend recovery. Browser tests should freeze or advance the browser clock
instead of sleeping to prove clock-change, resume and expiry behavior. Snapshot
reloads do not request provider refresh; only Refresh observations does.

Redemption requires the native policy enabled, fresh inventory, known available
credit ID, no read-only/writes-disabled state, no active request, and no pending
unresolved operation. Backend validation remains authoritative. Schedules accept
only timestamps with an explicit offset or Z; absolute expiry, normal weekly
reset and local scheduled redemption remain separate. Rollback removes these
routes/assets without touching credentials, pending operations, or the existing
management panel.
