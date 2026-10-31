# Upstream #7106 / #7112 backport

## Scope and baseline

This isolated candidate backports only:

- [`4cb98e3ccdc3852fb27f705358699a39a57d7a0a`](https://github.com/router-for-me/CLIProxyAPI/commit/4cb98e3ccdc3852fb27f705358699a39a57d7a0a), PR #7106: prevent deferred upstream request retention on Responses WebSockets.
- [`12d97cfe196be266b45f4f7e0cc394c9fdc2e099`](https://github.com/router-for-me/CLIProxyAPI/commit/12d97cfe196be266b45f4f7e0cc394c9fdc2e099), issue #7112: translate Claude mid-conversation `tool_addition` blocks.

The candidate is based on `9f3027ec7f0445c4679025c31623e11017f010d3`, the revision verified from the running production image on 2026-10-31. The initial `main` checkout at `d31824ec` did not contain the newer deployed fork fixes. Starting from the deployed revision retains existing fork commits `e8b4cab6`, `f2861b2b`, `6be26d0e`, and `9f3027ec`, including capacity recovery, authentication snapshot safeguards, private metadata filtering, owner recovery, and active reset protection.

The working branch is `backport/upstream-7106-7112`. The backport commits are `75051073` (#7106) and `4367b8aa` (#7112), retaining the upstream authors and source-commit provenance. Production and the main checkout were not updated.

## Integration and dependencies

Both patches applied cleanly against the production-aligned baseline. All upstream source and regression-test hunks are retained, including the new logging context helper and WebSocket logging test fixture. Reverse-application checks succeeded for both commits' source and test patches; the new context helper's Git blob matches upstream exactly.

No additional upstream commits, external modules, or `go.mod`/`go.sum` changes are required. Existing fork helpers and test fixtures satisfy the dependencies. Unrelated upstream commentary restoration, compression, usage expansion, and disconnect behavior changes were excluded. The fork's existing disconnect assertions remain alongside the complete logging test additions from #7106.

The implementation changes 14 Go files. Account selection, reset redemption, conversation binding persistence, quota journals, watcher behavior, deployment configuration, and payload-rule ordering are unchanged relative to `9f3027ec`.

## Behavior and regression coverage

- Established WebSockets carry an immutable, connection-scoped policy disabling error-only deferred upstream body capture before routing starts. The logging helper checks this policy before accessing pooled Gin state.
- Ordinary HTTP error capture, rejected upgrades, and full request logging remain covered. Tests include 40 MiB of repeated requests, delayed callbacks, cancellation, Gin context reuse, real WebSocket logging, model-router calls, and full-duplex Codex execution.
- Claude system-message tool additions become Responses `additional_tools` items or merge into Chat Completions tool declarations. Tool-result adjacency, tool references, escaped JSON tokens, duplicate names, long MCP names, and streaming/non-streaming name restoration are covered.

Negative controls ran with the new upstream tests and context helper but without either implementation change. They reproduced deferred body retention, missing Codex tool additions, missing long-name restoration, and missing Chat Completions tool additions. These were expected failures proving the regressions are detected.

## Validation

All Go commands use `scripts/go-limited.sh`, Go 1.26.10, half the available CPUs, and serial package compilation. Scratch files and build/module caches are separate from production credentials and policy state.

| Check | Result |
| --- | --- |
| Original checkout focused baseline | Passed, 5 packages |
| Production-aligned focused/account-policy baseline | Passed, 7 packages |
| Negative controls without fixes | Failed as expected in all 3 selected packages |
| Focused logging, middleware, WebSocket, translator, and utility tests | Passed, 7 packages |
| Full repository Go suite | 110 packages passed; `sdk/cliproxy` failed one intermittent concurrent-management test |
| Management test against original production implementation | Single test and full service package passed once; 50-run test reproduced the same HTTP 404 failure once |
| Broad relevant race checks | 20 packages passed; middleware and OpenAI handlers each failed one existing test |
| Original middleware under race detection | Same streaming-log data race reproduced with original implementation |
| Targeted upstream regressions under race detection | Passed, all 6 selected packages |
| Server compilation | Passed; verification binary removed |
| Formatting and patch whitespace | Passed |

The full-suite command was `./scripts/go-limited.sh test -count=1 ./...`. Raw logs are in `.tmp/backport-validation/` in this worktree.

The failing test is `TestPutOpenAICompatConcurrentWithAuthFieldsPatch_Issue6559`, in `sdk/cliproxy/service_openai_compat_race_test.go`. Concurrent PUT/PATCH calls occasionally return HTTP 404 from deprecated `/v0/management/openai-compatibility` and `/v0/management/auth-files/fields`; the full suite also reported the resulting missing final model prefix. This failure occurs without the backports: a Go source overlay restored all six modified existing implementation files to `9f3027ec`, and a 50-run repetition reproduced the same 404 once. The new context helper was unused in this baseline path. No deprecated endpoint, existing test, or routing code was changed to hide the failure.

Baseline reproduction command:

```sh
./scripts/go-limited.sh test \
  -overlay .tmp/backport-validation/baseline-overlay.json \
  -count=50 \
  -run '^TestPutOpenAICompatConcurrentWithAuthFieldsPatch_Issue6559$' \
  ./sdk/cliproxy
```

This is evidence of a pre-existing intermittent failure, not a root-cause analysis of the management race. It remains unresolved.

The broad race command covered logging, middleware, all runtime executors and helpers, all API handlers, the service, auth/routing, usage, both changed translators, utilities, account policy, durable bindings, ledger/history, watchers, and cross-module integration tests. Executors, custom auth/routing, usage, translators, policy, durable state, and watcher packages passed. Two tests failed:

1. `TestRequestLoggingMiddleware_StreamingPreservesFullUUIDForLogger`: the race detector reports `processStreamingChunks` reading `chunkChannel` at `internal/api/middleware/response_writer.go:257` while `Finalize` clears it at line 309. This implementation is unchanged by the backports. The same race reproduced with original production implementation under `-race -count=5`.
2. `TestResponsesLateInterruptDoesNotPoisonNextCreate`: an interrupt for response ID `unknown` reached the upstream after a terminal event, followed by a downstream close 1006. Attribution remains unresolved. The test passed 50 isolated runs under `-race` with the original production implementation, and it also passed in three repeated complete baseline handler suites. Those repeated baseline suites instead exposed `TestCodexClientModelsApplyPatchRouting` metadata mismatches on later repetitions (`apply_patch_tool_type=freeform` where nil was expected). No interrupt or model-metadata fix was added to this narrowly scoped candidate.

Baseline race commands:

```sh
./scripts/go-limited.sh test -race \
  -overlay .tmp/backport-validation/baseline-overlay.json \
  -count=5 -timeout=60s \
  -run '^TestRequestLoggingMiddleware_StreamingPreservesFullUUIDForLogger$' \
  ./internal/api/middleware

./scripts/go-limited.sh test -race \
  -overlay .tmp/backport-validation/baseline-overlay.json \
  -count=50 -timeout=90s \
  -run '^TestResponsesLateInterruptDoesNotPoisonNextCreate$' \
  ./sdk/api/handlers/openai

./scripts/go-limited.sh test -race \
  -overlay .tmp/backport-validation/baseline-complete-overlay.json \
  -count=3 -timeout=120s ./sdk/api/handlers/openai
```

The complete baseline overlay restores the original implementation and existing tests; files added by the backports become empty package declarations. Baseline comparisons never modify the candidate source or production files.

Targeted upstream race checks and server build:

```sh
./scripts/go-limited.sh test -race -count=1 \
  -run '^(TestRecordAPIRequest.*|TestResponsesWebsocketRequestLoggingLifecycle|TestResponsesWebsocketRejectedUpgradeKeepsErrorCapture|TestResponsesSteeringFullDuplexIntegration|TestRequestLoggingMiddlewareCapturesLargeErrorRequestAndDeferredAPIRequest|TestConvertClaudeRequestToCodex_MidConversationToolAdditions|TestConvertCodexResponseToClaude_RestoresLongMidConversationToolAdditionName|TestConvertClaudeRequestToOpenAI_MidConversationToolAdditionAppendsToTools|TestUnambiguousClaudeToolNames)$' \
  ./internal/runtime/executor/helps ./sdk/api/handlers/openai \
  ./internal/api/middleware ./internal/translator/codex/claude \
  ./internal/translator/openai/claude ./internal/util

./scripts/go-limited.sh build -o .tmp/backport-validation/server-check ./cmd/server
rm .tmp/backport-validation/server-check
```

The broad race invocation was:

```sh
./scripts/go-limited.sh test -race -count=1 \
  ./internal/logging ./internal/api/middleware \
  ./internal/runtime/executor/... ./sdk/api/handlers/... \
  ./sdk/cliproxy ./sdk/cliproxy/auth ./sdk/cliproxy/usage \
  ./internal/translator/codex/claude ./internal/translator/openai/claude \
  ./internal/util ./internal/accountpolicy/... \
  ./internal/accountpolicybindings/... ./internal/accountpolicyledger/... \
  ./internal/accountpolicyusage/... ./internal/watcher/... ./test
```

## Production safety and remaining issues

No production process, container, listeners, live configuration, credentials, reset automation, ownership bindings, or policy journals were changed. No server was started against production state. All executable verification uses isolated unit/integration fixtures.

Final verification confirmed the same production container ID, image ID, start time, and listener bindings as before the work. `/healthz` returned `{"status":"ok"}` on loopback, LAN, and Tailscale after validation.

There are no unresolved patch conflicts. The full test suite and broad race run are not green because of the failures documented above. This is a local backport candidate; it has not been deployed or pushed. No live-provider soak test was performed.
