# Account policy verification

Implementation branch: `feat/account-policy`; upstream base:
`8ef43e4df3b216a42493105d31c2873b69191473`.
Verification date: 2026-10-04. Isolated Go 1.26.0, Node 22, Chromium and the
Playwright CLI wrapper were used. No provider credentials or live resets were
used by the new fixture tests or browser preview.

## Acceptance evidence

| Requirement | Evidence |
| --- | --- |
| Exhausted weekly allowance spends earliest expiring reset now, with 11h remaining and no queued demand | Fake-clock core tests and real Codex adapter-to-scheduler fixtures |
| Later expiry/equal natural refresh stay saved; short-only or rounded100 with explicit permission do not trigger early spending | Core safety/provider integration regressions |
| Expiry guard remains fallback with used allowance; reserve/nonexpiring policy separate | Two-account scheduler/ordering and policy tests |
| Static priority cannot override earlier eligible deadline; stale/unknown evidence falls back; hold/reserve/force honor eligibility | Auth candidate/selector tests and 39 real HTTP/SSE/WebSocket matrix cases |
| Distinct UUID outcomes, journal before submit, one operation under concurrent calls, lost response/restart/passive reconciliation | Core journal/state-machine/concurrency tests |
| Invalid schema/owner disables writes, missing/null expiry distinct, unknown/purchased classes excluded, selected ID correct | Codex adapter fixtures, permanent-failure safety tests and API tests |
| `nothing_to_reset` and `no_credit` cannot create a polling/new-UUID loop | Core unchanged-evidence regressions |
| Verified updated quota/weekly deadline reranks and repairs only older affected persisted cooldown | Core recovery and transactional SDK recovery tests |
| Older evidence cannot erase new auth/quota failures; failed persistence remains retryable | Generation/timestamp and injected disk-failure tests |
| Active request/reset lease is atomic and includes true stream completion and retries | SDK runtime/lease tests; real synchronized SSE fixture |
| Strict ownership, safe-boundary migration, unsafe continuation, client isolation and child-parent registration ownership | Auth affinity tests and real transport continuation/reconnect cases |
| Same rules across HTTP/SSE/real downstream and upstream WebSockets; payload ordering unchanged | `internal/api/account_policy_transport_test.go`; existing executor payload/transport regressions |
| Authenticated v8 API only, atomic partial settings, bounded JSON, explicit IDs/time offsets, sanitized errors/diagnostics | Management and server API tests; proxyctl tests |
| Disabled defaults, existing selector identity, YAML round trips and hot reload, Home/plugin ownership conflicts | Config/watcher and SDK lifecycle regressions |
| Disabled unavailable journal/symlinked optional storage does not break ordinary proxy; journal bytes preserved | Lifecycle and usage disabled-startup regressions |
| Separate UI preserves downloaded panel; key stays in memory; no countdown polling | Maintained embedded UI audit, helper tests and Playwright checks below |
| Durable bounded secret-free usage, nullable unavailable metrics, safe dispatcher drain | Usage sink and SDK manager drain tests |
| Approved companion jobs, durable task states, isolated worktrees, worker/build capacity | Nine Python inbox tests |
| Private edge HTTP/SSE/WebSocket and operator gate rejects spoofed forwarding | Real temporary Caddy fixture smoke, including reconnect and nonoperator rejection |

## Playwright browser checks

The lead used the rebuilt current embedded assets at
`http://100.82.251.30:18318/account-policy.html` with fixture key `fixture-key`.
The preview accepts no credential files and has no live provider adapter.
CLI snapshots were captured before interacting with page elements. Assertions
ran against the real rendered DOM and fixture backend state.

Passed checks:

- Successful and incorrect-key login; disconnect removes account DOM. Key input
  is cleared and both localStorage/sessionStorage remain empty.
- Automatic write mode requires confirmation; policy, read-only, fallback,
  strict affinity and forced-account settings persist through the controls.
- Disabled and read-only settings leave zero enabled reset-write buttons.
- Unsorted source inventory displays October5/22/29 in expiry order while
  retaining opaque account-owned credit IDs.
- Selected October5 redemption confirms upstream-b/workspace-b and consumes only
  that credit. October22/29 and account A remain unchanged. Fixture usage becomes
  2% and updated weekly refresh/confirmed history are displayed.
- Scheduling rejects a naive timestamp. `2026-10-05T05:50:00+02:00` persists as
  `2026-10-05T03:50:00Z`; confirmation names the owner and selected credit.
  Cancellation removes the local schedule without changing provider expiry.
- Hold and15% reserve save. Local cooldown confirmation explicitly says it does
  not redeem provider allowance. Injected `schema_mismatch` is visible.
- Unknown expiry, confirmed nonexpiring, expired, stale/disabled and redeemed
  rows have distinct states. Ordinary weekly renewal and manual expiry labels
  are separate.
- Berlin October29 displays `18:48 GMT+1`; fixed GMT+2 displays `19:48 GMT+2
  (fixed)` for the same provider instant.
- With a controlled browser clock, countdown45s becomes42s after3s and fixture
  management request count remains51. Advancing through expiry disables the
  selected write control; visibility/pageshow events recompute state.
- Diagnostics download contains no management key/token fields. Reload/reconnect
  produced zero JavaScript runtime errors. Expected HTTP errors were deliberate
  unauthorized login/schema injection and fixture favicon401, not script errors.
- Desktop1440x1000 and mobile390x844 screenshots captured; mobile document width
  is390px, with no horizontal overflow. Existing management-panel link remains.

Local artifacts are preserved under `output/playwright/` and intentionally
ignored except the concise verification index. They include desktop/mobile PNGs,
DOM snapshots, CLI console records and sanitized diagnostic fixture JSON.

## Scope and remaining deployment gates

Local tailnet-interface transport fixtures do not prove access from remote
operator/worker devices. No production service installation, tailnet ACL/tag
mutation, remote-worker provisioning, production key setup or live credit
redemption occurred. Follow [the deployment guide](account-policy-deployment.md)
for explicit positive and negative remote-device tests before rollout.

Known limits:

- Distinct workspace identity is unsupported by the pinned provider protocol and
  disables writes; missing account identity and changed schemas fail closed.
- Locking platforms without the implemented safe file lock cannot perform
  durable reset writes. Run one protected writer/state directory.
- Some existing native WebSocket refusals close with1006 before a structured
  client error. Fixtures prove no unsafe business-payload replay.
- Cold stateful continuations without provable original owner fail closed;
  pre-policy cache entries cannot invent client provenance.
- The external panel's manual consume path is outside the native durable journal;
  use the maintained policy extension for selected durable redemption.
- Legacy schedules without stored upstream ownership fail closed rather than
  inheriting a newly mapped account.
- Permanent contract failures preserve uncertain UUIDs and inhibit further
  writes; repair/review is required, not automatic blind retry.

The original untracked specification and existing `.dockerignore` edit remain
untouched. No custom change was made to executors or translators. Integration
points and the upstream merge procedure are in
[the update guide](account-policy-upstream-updates.md).
