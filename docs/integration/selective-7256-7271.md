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
| `gofmt`, `git diff --check`, conflict markers, moego references | Clean |

Focused regressions exercised during integration: websocket 429 failover and
terminal error frames, early-429 retry without closing downstream, policy
refusal and filter-block classification, tool_search bridging, remote
compaction trigger and capsule expansion, Codex input ID sanitization, plugin
hot-reload quiesce failure and usage replay, count_tokens 501 neutrality,
usage queue slot release, and the account-policy transport, pinned capacity
wire and fallback fixtures.

## Independent verification

RESULT_VERIFICATION

## Deployment

RESULT_DEPLOYMENT
