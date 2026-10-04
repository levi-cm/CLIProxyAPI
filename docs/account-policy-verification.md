# Account policy verification

Implementation branch: `feat/account-policy`; upstream base:
`8ef43e4df3b216a42493105d31c2873b69191473`.
Verification date: 2026-10-04. Isolated Go 1.26.0, Node 22, Chromium and the
Playwright CLI wrapper were used. No provider credentials or live resets were
used by the new fixture tests or browser preview.

## Final verification gates

The final code was reviewed at `78287818`; subsequent edits are documentation.

- Full repository suite: PASS, uncached, using `go test ./... -count=1
  -parallel=1`, Go package concurrency1 and GOMAXPROCS4.
- Changed module/API/auth/config/UI/CLI/usage race tests: PASS. The new SDK policy
  lifecycle and legacy-disabled-selector race regressions are checked separately.
- Required `go build -o test-output ./cmd/server`: PASS; temporary binary removed
  and reproducible. `gofmt -w .`, `git diff --check` and focused `go vet`: PASS.
- JavaScript helpers:7/7 PASS. Companion:9 inbox tests and the real Caddy edge
  smoke PASS. Final real transport suite on100.82.251.30: PASS.
- Playwright checks below: PASS, including both numeric tailnet address and
  MagicDNS origin. `proxyctl accounts`/`explain` also succeeded against the private
  fixture listener. These are local-interface checks, not remote ACL proof.

### Upstream baseline failures observed

Parallel/fresh full runs intermittently failed the unchanged executor tests
`TestWebsocketRetryBindFailureClearsActiveSessionState` (4 connections instead
of3) and sessionless upload keepalive cases. An isolated connection-count rerun
passed; repeated connection-count testing also failed on pristine upstream
`8ef43e4d`. The final complete serial suite passed without altering these files.

Broad SDK `-race` runs are not consistently green: the unchanged
`TestHandleAuthUpdates_SameRevisionWaitDoesNotWaitForOtherAuthInBatch` and the
end-to-end auth replacement/patch model-list tests failed. All three identical
failures reproduced in the pristine upstream worktree; there was no data-race
warning. One isolated whole-SDK race run passed, but the broader repeat failures
remain an upstream baseline limitation. The feature-specific race gates pass.
No test was skipped, weakened, or edited to conceal these failures.

Read-only diagnosis: the legacy batch test blocks the second invocation of a
shared hook and assumes it belongs to B, although the unchanged task processor
uses concurrent workers. That can instead block A. Cleanup releases the barrier
without joining the batch before removing global model registrations, which
can explain the following model-list failures. This is an upstream test-ordering
issue, not a reason to refactor its processor in the feature branch.

Reproduce the baseline comparison in a detached worktree at the base commit:

```bash
go test ./internal/runtime/executor -run '^TestWebsocketRetryBindFailureClearsActiveSessionState$' -count=30
go test -race ./sdk/cliproxy -run 'TestHandleAuthUpdates_SameRevisionWaitDoesNotWaitForOtherAuthInBatch|TestEndToEndAuthFileReplacement_RestoresModelsInV1ModelsWithoutRestart|TestEndToEndAuthFilePatch_RestoresModelsInV1ModelsWithoutRestart' -count=1
```

Session logs are preserved under `/tmp/cliproxy-policy-final-*` and
`/tmp/cliproxy-policy-baseline-*`; the full-suite passing log is
`/tmp/cliproxy-policy-final-full-serial.log`. The broad race baseline limitation
must remain visible in future update verification; it is not a reason to accept
a newly failing custom regression or change unrelated upstream executors.

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

## Independent dashboard collection fix (2026-10-04)

Diagnosis: disabled deadline routing also disabled the private usage sink and
background quota discovery. Live provider read-only refresh succeeded for both
accounts (49% and 93% weekly remaining, five saved resets total); no reset was
consumed. The UI also hid quota after the 120-second action freshness limit even
though idle discovery normally runs approximately every five minutes.

The additive observations opt-in now enables discovery and durable completion
records independently of routing. Default-off behavior, routing/reset authority,
ownership, backoff and the upstream Redis queue are preserved. Startup config
controls the new flag across legacy journals. Last-known display values retain
honest stale labels; action freshness rules are unchanged.

Verification on the final code:

- Full `go test ./...` passed with Go 1.26, `GOFLAGS=-p=1`, `GOMAXPROCS=4`.
- Required `go build -o test-output ./cmd/server` passed; artifact removed.
- Focused race suites passed for account policy, SDK account-policy lifecycle,
  config, management, embedded UI and preview.
- All 20 JavaScript helper tests passed.
- `verify-dashboard.cjs` passed the 100-account/concurrent-activity/mobile/draft
  regression suite with no provider/reset/settings writes.
- `verify-observations.cjs` checks independent opt-in, zero retained attempts,
  waiting versus disabled collection, stale quota/inventory, no reset authority,
  and corrupt storage. The corrupt-storage case failed before correcting the
  renderer's health guards, then passed after rebuilding the preview.
- Authenticated Lighthouse snapshot accessibility was 100% in all four views,
  in both light and dark themes.
- Independent scope and quality reviews passed after the renderer correction.

Past unrecorded completions are not backfilled. Success rate, latency and token
graphs need real new usage evidence; unsupported token fields remain nullable.
Production verification uses read-only management/browser checks, not synthetic
usage records or billable inference requests.

## Playwright browser checks

### Per-account allowance and weekly-cycle timeline (2026-10-04)

The weekly average was removed. Individual account percentages remain visible
with last-known labels when stale. Ordinary seven-day cycles use rectangles
ending at the provider's observed refresh; start is inferred from the seven-day
duration. Shading represents elapsed time, not allowance consumption. The axis
includes the past week plus the selected future range. Manual expiry/guard lines
are on a separate lane; no future cycles or unknown provider values are invented.

- Three helper regressions failed before implementation, then all 23 JavaScript
  helper tests passed.
- Full Go suite and required server compile passed after the final UI fixes.
- Existing 100-account and observation-only browser suites passed against
  isolated fixtures, including unequal account allowances and unknown evidence.
- New `verify-weekly-timeline.cjs` proved elapsed geometry (three-sevenths of a
  cycle, not 67% quota used), expiry lines, no weekly line marker, all three
  future ranges at 1440/390/320px, keyboard account navigation and readable hover
  in both themes. The first run reproduced overlapping labels, inert allowance
  buttons and low hover contrast; all three passed after focused corrections.
- Lighthouse accessibility was 100% in four views in both themes. Browser
  verification made no provider/settings/reset writes.


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

Build-artifact scan found no stale generated repository directory meeting the
250MiB removal threshold. Verification evidence and all uncertain/existing
artifacts were preserved; no material build collection was deleted.

Known limits:

- Distinct workspace identity is unsupported by the pinned provider protocol and
  disables writes; missing account identity and changed schemas fail closed.
- Locking platforms without the implemented safe file lock cannot perform
  durable reset writes. Run one protected writer/state directory.
- Some existing native WebSocket refusals close with1006 before a structured
  client error. Fixtures prove no unsafe business-payload replay.
- Runtime diagnostics report observed downstream transport and honestly mark
  upstream transport unknown unless an executor reports it. Capability settings
  are not proof of actual transport; the fixture suite verifies real upstream
  HTTP/WebSocket handshakes separately.
- Cold stateful continuations without provable original owner fail closed;
  pre-policy cache entries cannot invent client provenance.
- The external panel's manual consume path is outside the native durable journal;
  use the maintained policy extension for selected durable redemption.
- Legacy schedules without stored upstream ownership fail closed rather than
  inheriting a newly mapped account.
- Permanent contract failures preserve uncertain UUIDs and inhibit further
  writes across healthy reads and restart. There is no automatic or one-click
  repair: intentional journal-guard maintenance after provider-contract review
  must preserve pending UUIDs. Healthy GET evidence cannot verify a changed
  consume contract, so it cannot silently restore write authority.

The original untracked specification and existing `.dockerignore` edit remain
untouched. No custom change was made to executors or translators. Integration
points and the upstream merge procedure are in
[the update guide](account-policy-upstream-updates.md).

## Account dashboard follow-up (2026-10-04)

The separate extension was redesigned without patching the downloaded upstream
panel or changing production policy configuration. An authenticated read-only
v8 dashboard endpoint exposes all registered accounts' local request activity
even when policy is disabled, and bounded retained usage-record aggregates.
Last selection and active execution are explicitly different. Retry/additional
model records are not presented as unique downstream client requests. Legacy
missing/zero token components remain unavailable rather than fabricated values.

Three independent read-only review seats checked backend correctness/privacy,
frontend lifecycle/ownership and publication scope. Findings were fixed and
re-reviewed: one-pass non-mutating binding sampling, sensitive header-fragment
alias redaction, nullable token evidence, runtime-only account countdowns,
whitelisted timeline advice, stable draft ownership and keyboard focus.

Final follow-up gates:

- Full uncached serial repository suite passed with Go 1.26 and package
  concurrency 1. A final complete serial run also checked the last targeted
  token-component fixes. Logs: `/tmp/cliproxy-dashboard-full-serial.log` and
  `/tmp/cliproxy-dashboard-final-full.log`.
- Focused dashboard/management/API/runtime tests and targeted concurrent
  usage/runtime/management race tests passed. Required server compilation,
  focused `go vet`, `gofmt`, Node syntax, Prettier and diff checks passed.
- 19 deterministic JavaScript helper regressions passed, including complete
  clock minutes on compact timeline axes.
- Real Playwright regression script passed against the fixture-only preview:
  100 accounts, ten pages, credential search and provider/status filters;
  simultaneous serving accounts; measured charts; collapsed/open account drafts
  surviving polling and display-zone changes; unsaved settings; stable keyboard
  focus on serving accounts and timeline markers; stale/503 recovery; mixed
  runtime-only providers; 390px/320px viewport without document overflow;
  memory-only key and cleared DOM on disconnect.
- The scheduled-credit draft regression switches October5 to October22 and
  changes display zone without reverting selected ownership or entered time.
  Visual control enablement is injected into a fixture read response only;
  no backend policy save or credit consumption is used for that check.
- Fixture counters stayed at zero provider refreshes, settings writes and reset
  writes during passive browser tests. No provider credentials were loaded.
- Authenticated Lighthouse snapshot accessibility audits scored 100 for all
  four sections in both light and dark themes (eight snapshots). These are
  automated accessibility checks, not a claim of exhaustive accessibility or a
  navigation/performance score. Mobile charts scroll within their own regions.

Repeatable browser and audit scripts are under
`internal/accountpolicyui/scripts/`; both refuse non-fixture preflight data.
Ignored screenshot evidence includes `dashboard-final-desktop.png` and
`dashboard-final-mobile.png`. Previous baseline-race limitations above remain
documented; no unrelated upstream test or production default was weakened.

## Nearby timeline and proportional allowance fills (2026-10-04)

The overview defaults to the next seven days, with a 24-hour zoom. Later saved
reset expiries no longer stretch the main timescale or add long timestamp lists;
a count links to the owning account's complete inventory. Weekly rectangle width
is still chronological. Opaque fill height now matches observed allowance left,
while a separate thin top strip marks elapsed time. Unknown measurements get no
fill and last-known observations remain labelled stale evidence, not authority.

Verification for this UI-only change:

- Two new deterministic regressions failed before implementation and passed
  afterwards; all 25 JavaScript helper/clock regressions passed.
- Full repository `go test ./...` and required server compilation passed using
  Go 1.26; final log: `/tmp/cliproxy-nearby-timeline-final-full.log`.
- Playwright verified the seven-day horizon, near expiry lines, later-expiry
  account navigation, opaque proportional fill, the independent elapsed strip,
  both range options and 1440/390/320px layout without overflow. A separate
  red/green browser regression checks the timeline itself has no clipped
  horizontal scroll area, with normal-size date labels on mobile.
- The 100-account dashboard suite passed. Observation-only checks use unequal
  49% and 93% allowances and measure the painted fill-height ratio in both themes
  at desktop and mobile widths. No provider refresh/settings/reset writes occur.
- All eight authenticated Lighthouse accessibility snapshots passed at 100.
- Read-only publication reviews checked quality and scope. All implementation
  changes remain in the optional dashboard module; no routing/reset policy,
  upstream defaults, downloaded management assets or private configuration changed.
