# Selective upstream integration: #7289, #7294, #7317, #7300, #7279

The local branch `integration/selective-7289-7317-20261107` starts at
`461f30f1`, the previously verified
`integration/selective-7256-7271-20261106` branch. Its application revision
`79501cba` is the target of pending release `204dec2c7f`. This preserves all
previously approved changes, including the serving `bc4ad493` baseline, in one
source lineage. No upstream branch merge or production checkout update is used.
The production checkout's two extra history-only commits are also accounted for:
`main` and ancestor `b8a73830` have the identical Git tree
`be3adc71d3acd7804486684bcf428a0541774972`; that ancestor's contents are retained.

## Selected changes and prerequisites

| Upstream commit | Purpose | Fork adaptation |
| --- | --- | --- |
| `1d4e054978160a5a702021d422c363c24a95aa51` | Preserve terminal request responses across local and Home execution, count, stream and bootstrap paths | Preserve the fork's capacity stop guard; do not wait for a Home redispatch acknowledgement on a stopped request |
| `68cd75724e4de4e0450475cc99c7491166cf3392` | Durable one-use Codex OAuth refresh claims and successful recovery receipts | Use existing gjson and encoding/json; retain file, Git, S3 and PostgreSQL journal implementations and tests |
| `ea0b471291a992c5d80358917dabc6cb9f0f25f6` | Claude OAuth state and scope compatibility | Use standard encoding/base64 and existing JSON libraries in regression tests |
| `2443e8830c401e6054370c109388bb3b59daf21c` | Nested custom tools and grammar declarations across Chat Completions round trips | Use allocating gjson/sjson operations; keep original input buffers immutable |
| `55c05f703302f9b4789117055464cdd70e221fa1` | Gemini tool-result role, pairing, ordering and thought-signature compatibility for Claude | Use standard encoding/base64 and existing gjson/sjson operations |

The last fix requires the shared role and snake-case tool-part helpers from
`c888ff72d46280202b51a5f635059ccd2dbb22e6`. Only its
`internal/translator/common/gemini.go` change and the Claude integration needed
by #7279 are included. Its unrelated Codex/OpenAI translator changes are omitted.
The incoming #7279 regressions cover omitted roles and both tool-part spellings.

The pre-existing SIWC identity/refresh machinery, file credential mutation gates,
store APIs, terminal-response types and Home test fixtures already supply the
other required interfaces. The upstream JSON-library migration is unnecessary.
The Claude management regression needs a small auth-service factory seam already
present in newer upstream. Only that seam is added, following the fork's existing
Codex test pattern. The test uses this fork's v8 provider query parameter; newer
upstream OAuth hint/re-login infrastructure is not imported. No deprecated route
registrations change. The shared v8 login handler retains its existing behavior.
`go.mod` and `go.sum` remain unchanged. Executor refresh tests use the fork's
existing `roundTripperFunc` test helper. The upstream no-copy allowlist hunk is
omitted because it only realigns entries for files absent from this fork.
Full `go vet` also found an unused `sync.Once` copied to `_` in the previously
approved plugin quiesce regression. The two dead lines are removed, without
changing its synchronization or assertions; that package is retested normally
and under the race detector.

No account-policy business rules, reset/request leases, credit idempotency,
pending reset UUIDs, durable conversation bindings, quota recovery, usage sinks,
configuration defaults, production credentials or state are modified.

## Verification

All Go commands run through `scripts/go-limited.sh`, Go 1.26.10, serial package
compilation, half-CPU affinity, `nice=15` and `GOMEMLIMIT=1400MiB`. Scratch files,
module/build caches, binaries and raw logs stay inside this project. Tests use
fixtures and do not mount or write production state or redeem live credits.

Raw evidence: `output/verification/selective-7289-7317/` in the project root.

| Gate | Result |
| --- | --- |
| `gofmt -w .`, formatting inspection and `git diff --check` | Passed |
| Full `go test -count=1 ./...` | Passed: 111 packages tested, 30 without test files |
| Race: bindings and client | Passed: 2 packages |
| Race: policy, usage/UI, API/management, service/auth/usage, CLI and preview | Passed: 10 packages |
| Race: Codex/Claude OAuth, stores, SDK auth, executors, affected translators and handlers | Passed: 10 packages |
| Plugin quiesce regression after dead-code cleanup | Passed normally and under race detection |
| `go vet ./...` | Passed after removing the inherited dead `sync.Once` assignment |
| Node UI and client tests | Passed: 38 UI tests and 1 client test |
| Python companion | Passed: 10 tests, 1 intentional skip |
| Separate Telegram companion | Passed: 85 tests |
| Python deployment controller | Passed: 88 tests |
| Python Go build-limit wrapper | Passed: 6 tests |
| Full server and all-command builds | Passed; verification binary retained in the evidence directory |
| CI catalog refresh and validation | Passed with `MODEL_CATALOG_DIR` pointing to scratch copies; tracked and live catalogs unchanged |
| Account-policy transport fixtures | Passed on loopback and the verified host Tailscale address `100.82.251.30`, using ephemeral ports |
| Real browser fixtures | Passed: 24 checks through the T3 shared browser, using the fixture-only preview |

The browser checks cover login, 100-account pagination/search, simultaneous serving,
graphs, drafts, stale/unavailable recovery, memory-only keys, 390/320px mobile
layouts, disabled/read-only policy, owner/workspace/selected-credit confirmation,
scheduling and cancellation, fixture-only selected redemption, ticking countdowns
without provider polling, mandatory automation locks and explicit-offset handling
for DST-ambiguous times. The fixture process and both task-owned browser tabs were
closed afterwards. No production provider was called or credit redeemed.

An extra LAN fixture attempt was rejected by the existing test helper, which
intentionally allows only loopback and tailnet addresses. This is retained in
`transport-lan.log` as an unsupported test invocation; its guard is not weakened.
The documented loopback/tailnet regression gates passed. Read-only production
health/model checks passed on all three interfaces, including LAN.

The optional real-PostgreSQL concurrency integration test remains skipped by its
upstream opt-in guard (no local PostgreSQL installation); deterministic PostgreSQL
journal and concurrency tests passed normally and under the race detector.
Initial import/helper integration failures and one transient existing XAI upload
test failure are retained in the initial logs. The corrected full suite and full
executor race run passed. No XAI implementation or test was changed.

GitHub-hosted PR checks are not claimed as run. The inherited upstream PR path
guard unconditionally rejects translator changes, and its AGENTS guard rejects
the historical AGENTS changes already in the local fork lineage. A PR against
the much older `origin/main` is not a clean five-patch publication path; no such
PR or main-branch rewrite is attempted. Local CI build/catalog checks passed.

Independent Codex verdict: **source approved, no blocking source findings;
deployment blocked**. The verifier reviewed all five original upstream hunks,
the prerequisite subset, current-task diff and full combined diff against the
serving baseline. It independently reran eight affected packages, full
`go vet ./...` and diff checks, all passing, and checked the two import-formatting
corrections. It confirmed that protected policy, binding, usage, configuration,
default and lease adapter paths are unchanged even against the serving baseline.
The final server was rebuilt successfully after the formatting corrections.
The complete independent artifact is
`output/verification/selective-7289-7317/independent-review.md` in the project root.

## Deployment readiness and pending release

The existing controller is in `ops/idle-deploy/` in the earlier pending checkout.
At initial inspection on 2026-11-07 it was observing traffic for release
`204dec2c7f`, with production still serving `bc4ad493`. No deployment transaction
had begun. Controller files are byte-identical between the pending checkout and
the combined candidate.

Verified protections include passive HTTP parsing and capture freshness/drop
gates, a five-minute idle period, active request and connection rechecks,
admission rules on host and container interfaces, a global exclusive watcher
lock, per-release worker/commit/journal locks, reset redemption exclusion,
policy/history writer fencing, immutable artifact/configuration hashes, fixture
handoff/rollback rehearsals and an independent rescue watchdog. Current state
and pending operation identifiers are retained during rollback.

However, this is a one-shot maintenance controller, not a zero-downtime handoff:
`core.Cutover.run` gates admission, stops the old process and only then starts
the candidate. Its README explicitly permits an interruption below 40 seconds.
The pending manifest also allows retirement of dormant client sockets. It has no
persistent deployment queue, release coalescing, or idempotent enqueue API;
the watcher lock prevents simultaneous workers but does not supply those features.

The current task's global AGENTS instructions require zero downtime and preserved
WebSockets. A queue extension alone cannot make this stop/start deployment meet
that requirement. Therefore no new release is prepared, armed, enqueued or
deployed, and no pending manifest, artifact or running watcher is modified.
The combined source candidate is retained for a future verified handoff. The
previous working image remains available. Post-deployment checks cannot be
reported as passed because this candidate has not been deployed.

There is an additional rollback protocol gate: the previous working `bc4ad493`
image predates the durable Codex OAuth refresh journal. As documented in
`docs/codex-refresh-recovery.md`, older binaries and external refreshers cannot
coordinate these claims. Retaining the old image alone does not prove safe
rollback after an ambiguous refresh or a crash before credential persistence.
A rollback binary that honors the same journal protocol, plus fixture recovery
proof, is required before deployment; unresolved claims must never be discarded.

Final read-only production checks passed health/models on loopback, LAN and
Tailscale, the exact serving revision, usage-history health and mandatory refills.
The container and working image were unchanged, restart count remained zero,
configuration hashes matched the earlier manifest, Home remained disabled, and
release `204dec2c7f` remained `waiting_for_idle` without a transaction. The new
source candidate is neither a queued deployment nor a deployed release. No
post-deployment continuity result or remote-device ACL proof is claimed.
