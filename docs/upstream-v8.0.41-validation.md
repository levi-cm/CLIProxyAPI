# Upstream v8.0.41 integration validation

The complete pinned upstream merge is staged on `integration/upstream-v8.0.41`.
Source candidate: `8cd69e8860d841b3a32fd822eb708273b1d983b6` (merge `37a7e52b`
plus Claude payload-precedence and active-response interrupt corrections).
Upstream parent: `8289ec4c71ad914d0612c2776352b76ba3d355a6`.
Fork baseline and recovery ref: `2896a388b2d43006477a5e4cb3670cce73c544f5`,
`recovery/pre-upstream-v8.0.41-20261023`.

**Production deployment is blocked. Local main and the serving router remain at
2896a388.** This is a source integration candidate, not a production acceptance.

## Deployment blocker

The isolated readiness rehearsal used 90 MiB (94,372,243 bytes) of synthetic raw
history: 96,991 requests across six segments, with derived lookup data rebuilding.
The retained rollback image failed its 40-second readiness limit. A diagnostic
retry remained in initialization without a ready listener at 135.275 seconds.
Production policy/history state measured 85 MiB at the time of the check.

This failure does not establish that warm production indexes would fail startup.
It does establish that the required startup/rollback margin has not been proved
for this realistic-volume recovery case. The plan requires candidate acceptance
by 25 seconds and full upgrade or rollback within 60 seconds; no maintenance
window or production cutover was attempted. No production state was mounted by a
test container, and no second production writer was started.

Before proceeding, prove bounded lookup/index recovery or an independently
validated compatible prebuilt lookup, then repeat upgrade and forced-failure
rollback with current durable state, writer exclusion, admission gating and
connection draining. Preserve raw records and current reset/ownership state;
never restore stale journals to make a rehearsal pass. Keep automatic redemption
mandatory throughout. The existing server closes connections on shutdown.

## Conflict and compatibility decisions

| Surface | Resolution |
| --- | --- |
| Management handler | Retain upstream SIWC factory/commit lock and fork policy, validation and usage fields. |
| Credential lifecycle | Retain credential-version/registration-epoch fencing, stale refresh patches and file-snapshot protection. Carry policy refresh notifications through post-commit effects after mutation release; discarded stale writes do not notify. |
| Selection | Preserve dynamic all-priority policy candidates while using upstream's caller-supplied scheduler mode outside the manager lock. |
| Streams | Combine execution sequence/cooldown behavior with request leases retained until the stream finishes. |
| Affinity | Preserve exact bound/parent/unavailable hooks and caller/provider/model namespaces. Scoped guardians use exact parent validation; ordinary upstream guardian behavior remains available to ordinary selectors. Unknown encrypted owners still fail closed. |
| Builder | Retain upstream runtime configuration cloning plus policy initialization and extension wiring. |
| Usage | Combine native-key provenance/effective tier with measurement presence, response status, requested/execution/response models and usage drain. Preserve separate cache reads/writes and raw history retention. |
| Partial token accounting | Preserve explicit zeros and absent fields; distinguish cumulative earlier totals from current partial totals; retain inconsistent Claude reasoning instead of synthesizing complete measurements. |
| Payload precedence | Preserve user rules once, last. Move Claude prompt-cache cleanup before final rules across execute, stream and local/native token-count paths. |

Additional compatibility fixes:

- Close only a rebalance RPC's own socket when its context is cancelled; the Redis
  pool's Close no longer reliably interrupts its checked-out blocked read. This
  uses cancellation, not a lasting network deadline, and preserves the source
  command pool and heartbeat fencing.
- Recognize go-git alpha.5 missing object-pack filesystem errors as corruption.
  Corruption fixtures reopen repositories to avoid cached objects concealing disk
  damage; dirty managed files still fail closed.
- Capture Codex request classification before asynchronous execution. This avoids
  reading a reused Gin request after streaming and preserves non-SIWC eligibility
  rules for Codex clients.
- Remove xAI version-lookup request/client timeouts that survive connection
  establishment. Upstream TLS-handshake bounds end at handshake completion and do
  not leave post-connection socket deadlines.
- Synchronize WebSocket fixture connection numbering before the handshake and wait
  for the uploaded request before the mock sends terminal completion. The previous
  fixture ordering raced under load and the race detector.
- Add the fixture preview's validated `-now RFC3339` option. The weekly browser
  fixture's fixed October examples otherwise drift as the real date advances.
- Bind local HTTP-fallback interrupts to the identified active response. Clear
  terminal state before writing completion downstream so late/unknown IDs cannot
  cancel a new turn. Deterministic identity tests and 20 race-enabled late-interrupt
  integration repetitions pass.
- Correct deployment/integration documentation that recommended disabling mandatory
  automation or routine production restarts.

## Validation

Final source checks are recorded below after their commands complete. Detailed
logs remain in the integration worktree's `.superpowers/sdd/upstream-v8.0.41/`.

| Check | Result |
| --- | --- |
| Baseline `go test ./...` | PASS on 2896a388. |
| Go toolchain | Go 1.26.10 for candidate validation and builds. |
| Final `go test ./...` | PASS, 111 tested packages. |
| Affected race suites | PASS, 25 tested packages. Covers policy, bindings, client, ledger, usage, UI, APIs, management, Home, stores, executors/helpers, watchers, WS relay, SDK/auth and operator commands. |
| JavaScript and client-header suites | PASS, 38 tests. |
| Companion fleet suite | PASS, 10 tests with one private-edge smoke skipped without CADDY_BIN. |
| Independent Telegram companion | PASS, 85 tests on a source-only fixture copy; original dirty nested repository unchanged. |
| Desktop/mobile dashboard | PASS: authentication, 100-account pagination, concurrent serving, graphs, drafts, focus and stale/failure recovery. |
| Desktop/mobile history | PASS: daily allowance rows, 24 override durations, single/split apply/cancel, 50/50 and 1/99 and 99/1 splits, model identities and cache percentage. |
| Mandatory automation controls | PASS: locked stale drafts, guard minimum, invalid-change rejection and no unexpected writes. |
| Observation-only UI | PASS on isolated fixtures; no reset authority or provider writes. |
| Weekly timeline | PASS with `-now 2026-10-01T10:00:00Z`: theme/mobile geometry, clock/navigation and no writes. Live-clock runs initially failed because dated examples drifted. |
| Focused regressions | PASS after observed failures: caller/model guardian isolation and parent validation; tier retention; presence/partial/reasoning semantics; Gin reuse; Claude payload overrides; Redis cancellation; Git corruption. |
| WebSocket fixture repeats | PASS: 20 retry repetitions and 10 race keepalive repetitions. |
| Formatting/diff checks | Go 1.26.10 gofmt and git diff --check clean. |
| Fresh independent review | One P2 payload-precedence finding, fixed with failing-then-passing regressions; no other actionable issue identified. Static review limits apply. |
| Server build | PASS: Go 1.26.10, clean source archive, required server build; generated test-output removed from source after retaining the release binary. |
| Isolated release container | PASS on final code: embedded commit, authenticated models, mandatory automation, mock inference, complete SSE, wrong-key 401, smoke history capture and healthy archive. |
| Upgrade/rollback rehearsal | FAILED readiness safety gate described above. |

The final race rerun uses frozen source. The preceding broad run exposed the
late-interrupt identity bug; its regression was reproduced and fixed before the
final successful broad rerun. The fresh static review preceded that last
validation-discovered correction; its new tests and both complete suites pass. An earlier intermediate race run was
invalidated by a concurrent test rename while Go generated its runner; it is not
counted as passing. Earlier genuine test/race failures are documented above with
their fixes and retained logs.

The plugin native ABI stays at version 1 and RPC schema at version 6. SDK/plugin
fixtures cover the additive callback and decoded-body contracts. The installed
gateway plugin and remote gateway provider remain disabled; their live network
operation is not certified. No separate CLIProxyAPIHome checkout was available
for a companion-side wire compatibility check.

## Production evidence and unavailable acceptance

The serving container remains `94eef1aafb049f49487c44d3180db299b6fb36ba3208a41d2f07f03d7ed75c78`,
image `cliproxyapi-fork:allowance-2896a388`, image ID
`sha256:71bcba6b033151e90196c6e1afe42a61003e1b12fe3262c7bdb31e0764b9a761`.
Its original start time is `2026-10-23T17:59:01.013533949Z`, with restart count zero.

Host self-checks pass over loopback `127.0.0.1`, LAN `192.168.178.156` and Tailscale
`100.82.251.30`: health, both UIs, authenticated models (13), policy diagnostics,
mandatory automatic redemption and expected unauthenticated model-list 401.
No serving process, container, listener, configuration, credential, journal,
conversation binding or catalog was changed by this update task.

The original modified `.dockerignore` and five untracked files remain preserved.
No source was pushed. No automatic credits were redeemed for validation.

Because deployment did not proceed, production upgrade/rollback interruption,
admission/draining, candidate-written production-state compatibility, fresh live
inference/SSE/WebSocket/reconnect and pre-cutover genuine encrypted continuation
acceptance were not certified. The planned consistent release backup/restore and
30-minute post-cutover monitoring were not performed. Those are remaining gates,
not implied successes. No authorized separate remote client was supplied; host
self-connections do not certify a different device's LAN/Tailscale path.

## Rulings recorded during execution

- Use the latest plan's explicit 60-second maintenance ceiling, including rollback,
  only after a successful rehearsal. Risk if wrong: production interruption. No
  maintenance window was taken.
- Use the previous commit author's identity for per-command Git commits, without
  changing shared/global Git configuration. Risk if wrong: attribution only.
- Do not cut over after the failed readiness rehearsal. Cost: deployment and live
  acceptance remain incomplete.
- Keep local main at 2896a388 because the plan places the failed rehearsal before
  advancing main. Cost: source integration waits on the isolated branch.

## Retained isolated release artifact

Image: `cliproxyapi-fork:upstream-v8.0.41-8cd69e88`.
Image ID: `sha256:2a2629d22c9d7e2afeabb90bd2b02441f2e8685937956bd1f12826c2341b731c`.
Embedded source commit: `8cd69e8860d841b3a32fd822eb708273b1d983b6` (verified through the running
fixture management response). Fixture readiness after docker run returned was
0.099 seconds with a fresh empty fixture state.
That smoke result does not supersede the realistic-volume recovery blocker.

The clean source manifest, built binary, fixture smoke/rehearsal scripts and JSON
results are under `.superpowers/sdd/upstream-v8.0.41/release/` in the integration
worktree. Generated artifacts were scanned; no obsolete equivalent met the
250 MiB deletion threshold. Current artifacts, release archives and rehearsal
evidence were retained. No global/shared caches were removed.
