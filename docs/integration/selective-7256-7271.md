# Selective upstream integration: #7232, #7233, #7242, #7248, #7254, #7256, #7267, #7271

This branch (`integration/selective-7256-7271-20261106`) starts from the
production revision `bc4ad493d1309764aab340ca7c8ab477aec0aff5` (the serving
image `cli-proxyapi-fork:claude-limit-bc4ad493`) and applies the requested
upstream fixes one patch at a time. Older upstream `main` history is not
merged. Every cherry-pick keeps the upstream author and carries a
`(cherry picked from commit ...)` provenance line.

## Integrated commits

| Order | Upstream | Fork commit | Scope |
| --- | --- | --- | --- |
| 1 | `90c447d3` (prerequisite) | `d015b266` | `tsc_` item ID prefix for Gemini tool search calls; the duplicate-aware collision map that `606e667a` edits |
| 2 | `606e667a` (#7233) | `86a0b1fd` | Sanitize invalid characters in Codex input item IDs |
| 3 | `6e103599` (#7232) | `dffc8cce` | Abort plugin hot reload on quiesce failure and coordinate usage replay |
| 4 | PR #7242 merge `6897eebd`: `c3938b72`, `0a94b2ae`, `69a032de`, `03a8743c`, `508ac2bc`, `be691624` | `a612fbef` | Classify mid-stream Gemini filter blocks and Claude policy refusals as 400 request faults and persist error logs, including the PR's follow-up fixes |
| 5 | `dbb70621` (prerequisite) | `7e4f1653` | Expand recognized CPA compaction capsules before forwarding to OpenAI |
| 6 | `d57270f4` (#7248) | `352c329a` | Codex remote compaction v2 on OpenAI-compatible routes |
| 7 | `8c9a7851` (#7254) | `a86666f8` | Bridge client `tool_search` in the Chat Completions Responses translator |
| 8 | `86a1aabf` (prerequisite) | `2ed82ac2` | Retry another credential on early websocket 429 without closing downstream |
| 9 | `b5e6027c` (#7256) | `0e0b4ccf` | Expose terminal 429 error frames on the Responses websocket |
| 10 | `f2e255f7` (#7267) | `7f3ac74f` | Clear the consumed queue slot in the usage manager dispatcher |
| 11 | `1c537211` (#7271) | `79501cba` | Keep count_tokens 501 and unsupported capability responses availability-neutral |

`585f97de` (follow-up to #7256) only threads request context and client
headers through `availableAuthsForSelector`. That request-aware signature
comes from upstream `dfe8b182` (SIWC bypass for image requests), which this
fork does not carry; the fork's selector availability is not request-scoped,
so the fork's `HasAlternativeTargetForModel` already passes every argument its
signature accepts. The follow-up is therefore covered without a separate
patch.

PR #7242 is integrated as its merge commit rather than only `c3938b72`: the
later commits in that PR correct the first one (stream-level evidence before a
Gemini filter block is classified, Claude Fast status preservation, no 200 OK
body buffering, non-stream refusal classification). Taking the first commit
alone would have shipped the defects those follow-ups fix.

## Fork adaptations

- JSON access stays on `gjson`/`sjson`. Upstream migrated to `moego/ojson`
  after this fork's base; every incoming hunk was translated back and no module
  change was introduced.
- Antigravity non-stream execution keeps the fork's goroutine scanner; the
  deferred filter-block classification runs before usage-metadata filtering,
  as upstream does in its stream path. The Claude non-stream executor keeps
  per-line usage observation because the structured SSE usage observer is not
  in this fork.
- `HasTrustedRetryDeadline` is implemented over the fork's existing Home,
  unavailable, model-cooldown and safe-header carriers (the fork never adopted
  upstream's trusted retry-after annotation chain). `SafeResponseHeaders`
  forwards an executor-parsed reset hint for terminal 429s as a bounded
  `Retry-After`, which is what #7256's regression tests require.
- The Codex websocket stream executor combines upstream's "do not notify
  disconnect before payload was emitted" rule with the fork's capacity guard,
  so the fork's bounded capacity recovery keeps its own no-notify path.
- Helpers that only exist in newer upstream are omitted where they have no
  fork equivalent: the original-request context accessor, the
  finalized-response-format metadata key, the Anthropic credit-balance
  classifier, the structured plugin stream error tests from `35e834da`, and
  the Antigravity chunk-assembly test that targets upstream's newer assembler.
- The usage retention test probes the record's `Generate` pointer instead of
  upstream's cache-detail pointer, which this fork's usage record lacks.
- API server tests wait for hijacked websocket handlers before their temporary
  auth directory is removed. Error-only request logs for websocket sessions
  now complete after the client closes, which otherwise races the
  `t.TempDir` cleanup.

No account-policy, routing, cooldown-recovery, request/reset lease, reset
journal, usage sink, conversation-binding, configuration default or deployment
controller code was changed. `go.mod`/`go.sum` are unchanged.

## Validation

All Go commands use `scripts/go-limited.sh` with the project toolchain
(Go 1.26.10), half the CPUs and serial package builds. Scratch state lives in
`.tmp/selective-7256-7271/`; raw logs are in `.tmp/selective-7256-7271/validation/`.

| Check | Result |
| --- | --- |
| Full Go suite (`go test ./...`) | Passed, 111 packages; 30 packages without tests; 0 failures (`go-test-all.log`) |
| Race: `internal/accountpolicybindings`, `internal/accountpolicyclient` | Passed, 2 packages, no data races (`race-bindings.log`) |
| Race: account policy, API, management, cliproxy, auth, usage, proxyctl, preview | Passed, 10 packages, no data races (`race-core.log`) |
| Race: pluginhost, clienterror, logging, middleware, handlers, executors, Responses translator | Passed, 11 packages, no data races (`race-integration.log`) |
| Node: `internal/accountpolicyui` | Passed, 38 tests |
| Node: `client-usage/codex-auth-headers.test.mjs` | Passed, 1 test |
| Python: `companion` | Passed, 10 tests (1 skipped) |
| Python: `ops/idle-deploy` controller | Passed, 88 tests |
| Server build (`cmd/server`) | Passed; verification binary removed (`build.log`) |
| Immutable image | `cli-proxyapi-fork:selective-7256-7271-79501cba`, `sha256:9ed6580849d73bbc8fec413c3bf8877d8bdd0d9f797c721b85c82eaaade57255`, built from `79501cba` with matching `COMMIT` build argument and revision label (`docker-build.log`) |
| `gofmt`, `git diff --check`, conflict markers, moego references | Clean |

Focused regressions exercised during integration: websocket 429 failover and
terminal error frames, early-429 retry without closing downstream, policy
refusal and filter-block classification, tool_search bridging, remote
compaction trigger and capsule expansion, Codex input ID sanitization, plugin
hot-reload quiesce failure and usage replay, count_tokens 501 neutrality,
usage queue slot release, and the account-policy transport, pinned capacity
wire and fallback fixtures.

## Independent verification

An independent read-only review agent compared every requested upstream
commit hunk by hunk against the candidate (normalizing the JSON-library
translation), checked payload-rule ordering at every compaction insertion
point, confirmed no changes under the account-policy, lease, binding,
journal, configuration or deployment-controller paths, re-ran 15 affected
packages (all passing) and `go vet`, and verified the recorded logs.
Verdict: approved, no blocking findings. Non-blocking notes:

- The new terminal-429 `Retry-After` is derived from the executor's parsed
  reset hint and is not capped, matching upstream's trusted annotation; a
  usage-limit reset far in the future surfaces as a large value.
- Websocket error-frame header precedence keeps the fork's rule (safe headers
  override passthrough headers on collision); the only key that can collide
  today is a cooldown `Content-Type`, so there is no practical divergence.
- Expect more `error-*` request-log files for terminal upstream websocket
  failures, as in upstream.

## Deployment

The existing idle-deploy controller (`ops/idle-deploy`, byte-identical to the
checkout that performed the previous release) was used from this worktree.
No new deployment process was introduced.

- Candidate: `cli-proxyapi-fork:selective-7256-7271-79501cba`
  (`sha256:9ed6580849d73bbc8fec413c3bf8877d8bdd0d9f797c721b85c82eaaade57255`),
  source commit `79501cba`; serving revision at preparation `bc4ad493`.
- Prepared release `204dec2c7f` (manifest under
  `/DATA/AppData/CLIProxyAPI/.tmp/idle-deploy/204dec2c7f/`). Isolated
  rehearsals on copied state: candidate start 12.6 s, rollback start 3.5 s,
  3,135 owners preserved on both paths, mandatory refills and history health
  confirmed, complete fixture interruption 5.0 s for the success cycle and
  10.6 s for the forced watchdog rollback (5.4 s recovery), all inside the
  40-second budget. A genuine pre-deployment encrypted continuation proof was
  recorded.
- Dormant-client retirement is pinned, as in the two previous approved
  releases; the five-minute quiet window, zero authoritative active requests,
  frozen admission, writer exclusion, mandatory automatic resets and the
  independent rollback watchdog remain required.
- Watcher `cliproxy-idle-deploy-204dec2c7f` was armed on 2026-11-06 at
  14:12 CET without `--now`. Initial status: `waiting_for_idle`, capture
  healthy, no pending parser state, no capture drops. Production was still on
  its original container and image at that check.

Public status and results are published under
`/DATA/AppData/CLIProxyAPI/output/verification/idle-deploy/204dec2c7f/`
(`status.json`, then `deployment.json`, `live-verification.json`,
`verification-status.json` and `codex-verdict.json` after the cutover).
`operator-approval.json` records the authorization for this release. The
controller verifies interfaces, a genuine encrypted continuation on its
original owner, SSE, native websocket turns, history and mandatory refills
after reopening admission, then runs the independent Codex verifier. A failed
readiness check before admission reopens rolls back automatically.
