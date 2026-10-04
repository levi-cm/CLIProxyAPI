# Maintained account policy panel

The existing management panel remains at `/management.html`. This package adds
the native optional policy controls at `/account-policy.html`, with embedded,
maintained CSS and JavaScript. It obtains snapshots and writes only through the
authenticated `/v8/management/account-policy` APIs. Countdown ticks never fetch
the provider or management API. The management key is held in page memory only;
disconnect clears it. The inference API key is separate.

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
node --test internal/accountpolicyui/clock_test.cjs
go test ./internal/accountpolicyui ./cmd/account-policy-preview
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
