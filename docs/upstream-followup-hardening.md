# Follow-up upstream hardening

## Scope

Implemented on `backport/upstream-7106-7112`, starting at `614ff727`, after the #7106 and #7112 backports. This candidate has not been deployed or pushed. The production router, live configuration, credentials, policy state, and durable bindings are not used as test fixtures.

| Upstream commit | Integrated behavior |
| --- | --- |
| `477605dc` | Defer request capture until authentication, release incomplete/rejected inbound requests, bound initial HTTP/RESP authentication, and recover from transient descriptor exhaustion |
| `ad8bdd3f` | Bound zstd log decompression output and decoder window |
| `3e7a21e2` | Keep duplex reauthorization out of shared Gin state and enforce the original client principal |
| `b7442218` | Capture request identity and client classification before asynchronous work |
| `9717e325` | Reject unknown or terminal WebSocket interrupts and preserve every claimed frame for an answer |
| `f013eb66` | Preserve an incomplete accepted stream's 502 without credential cooldown or automatic replay |
| `aa4130e6` | Required truncation classification across executors, manager results, usage, and queue records |
| `6340fe43` | Preserve the requested client model for duplex authorization after interceptor rewrites |
| `f73e66ac` | Required disconnect/turn ownership support for interrupt handling, including no replay and queued-input fencing |
| `fa42f390` | Assert the disconnect error event explicitly in the duplex integration test |

Upstream regression tests are included. The unrelated full-chain compression feature and scheduler selection-ID expansion were not imported.

## Fork integration

- Kept the early client-classification snapshot before custom policy tracking, using the upstream context API underneath it. Kept stream policy leases and their release boundaries.
- Added stream-truncation classification alongside the fork's capacity classification. Existing owner recovery, quota/reset fences, credential-version checks, and hard policy exclusions remain effective.
- Kept the fork's stricter local interrupt response-ID matching. Terminal events stop new claims while already claimed frames remain available for an explicit answer. Upstream queue tests now establish a matching response ID, and the saturation test submits two frames for the same response, so rejection is actually caused by the full queue.
- Preserved the custom capacity stop conditions when adding upstream WebSocket loss handling. Lost turns are not automatically replayed, and stale queued inputs are rejected without changing the conversation's account owner.
- Omitted the `response_compression.go` unwrap-only hunk because those compression wrappers do not exist in this fork. Existing wrappers that participate in request cleanup have the required unwrapping behavior.

The incoming HTTP header limit and initial RESP authentication limit operate before upstream execution. No upstream request, read, write, or idle timeout was added. Request cleanup skips consumed bodies and hijacked connections; a regression verifies accepted HTTP streaming and hijacked connections receive no cleanup deadline.

The truncation failure code is an additive usage/queue field matching upstream. Home lifetime/ownership behavior is covered by local integration fixtures. A separate CLIProxyAPIHome checkout was not available, and no external Home service was changed or deployed.

## Local fixes

### Streaming-log channel race

The asynchronous logging goroutine previously read `w.chunkChannel` while `Finalize` cleared that field. It now receives immutable channel and writer references when it starts. Finalization still closes the channel and waits for queued chunks to drain before closing the log writer.

The existing logging test reproduced the race before the fix and passed 20 repeated race checks afterward.

### Management reload during in-place writes

The watcher first hashes the file and then loads it with a second read. An in-place save can truncate the same inode between those reads. The loader accepts an empty file as defaults, which can clear the management secret and other runtime settings.

The watcher now rejects the empty source snapshot before publishing it, retains the last good configuration, and leaves the change hash uncommitted so the completed write can still reload. A deterministic test truncates the fixture file between the two reads, then verifies the completed write is applied. This preserves bind-mounted file identity and does not change deprecated `/v0/management` handlers.

### Repeated model-catalog test isolation

The apply-patch routing test asserted defaults for every model in the global registry, including native models registered by other tests. It now asserts only the models declared by its fixture. All of its enabled, disabled, mixed-provider, alias, and native-route expectations remain covered. Production catalog behavior is unchanged.

### HTTP interrupt cancellation fixture

A repeated handler race run exposed an immediate atomic-flag assertion after the interrupt response. The backend observes cancellation asynchronously, so receiving that response does not prove the fixture goroutine has already run. The test now waits for the backend cancellation channel, with a five-second failure bound. It still fails if cancellation never arrives; production cancellation behavior is unchanged. The corrected test passed 50 race-enabled repetitions.

### Review: terminal channel ordering

Independent review identified an interaction between the buffered terminal-error channel and the data channel closing. Both channels can be ready simultaneously, so the forwarder could choose closed data first and close the downstream socket without reporting the upstream loss.

The forwarder now drains a pending terminal error before treating data closure as clean EOF, using the same error-handling path for both cases. A real WebSocket regression prepares both channels as ready, checks the correlated loss frame, and then sends another frame to prove the connection remains usable. Both duplex and nonduplex cases are covered without sleep-based ordering.

### Review: configured error rules and truncation

Independent review also identified that a configured 502 `stop-and-cooldown` or `continue-and-cooldown` rule could replace the local truncation marker with `force_cooldown`. Typed local truncation errors now bypass rules intended for received upstream refusals. A known-owner regression checks both cooldown actions and verifies a genuine upstream refusal still matches the configured rule.

## Validation

Go 1.26.10 is invoked through `scripts/go-limited.sh`, with half the CPUs, serial package compilation, and project-local scratch/cache directories. No test server points at production credentials or account-policy state.

Before applying fixes, regression controls reproduced:

- Streaming-log channel access racing finalization.
- Duplex reauthorization replacing another request's identity.
- Delayed usage publishing with a reused request's API key/provider.
- Empty in-place config writes replacing the last good configuration.
- Accepted truncated streams cooling the credential pool and becoming `auth_unavailable`.
- Interceptor-rewritten client models bypassing a denied alias on continuation.
- Unknown response interrupts reaching the upstream socket.
- Incomplete unauthenticated requests remaining open and transient accept errors terminating the listener.
- A 40 MiB decoded log body exceeding the unconfigured 32 MiB bound.

Additional negative controls restore the old cooldown classifier or remove the hijack guard through Go source overlays. They demonstrate that the new owner and connection-lifetime tests detect those regressions without editing the candidate or production.

| Check | Result |
| --- | --- |
| Focused suites across API, middleware, executors, handlers, auth, usage, watcher, and queue | Passed, 12 packages |
| Known-owner accepted-stream and accepted-connection lifetime tests | Passed, 2 packages |
| Independent review regression fixes | Both reproduced before correction and passed afterward |
| Full Go suite | Passed after all fixes, 111 packages; 30 packages without tests |
| Repeated management race check | Passed, 50 repetitions |
| Repeated WebSocket/handler race check | Passed, entire OpenAI handler package, 3 repetitions |
| Broad race checks | Passed, 27 packages with tests; 1 package without tests; no race reports |
| Server build | Passed; verification binary removed |
| Formatting and whitespace | Passed, all 68 changed/new Go files; no conflict markers |
| Production identity and network health | Unchanged container/image/start time/port bindings; healthy on loopback, LAN, and Tailscale |

Detailed logs and exit codes are retained under `.tmp/backport-validation/followup-*` and `results.tsv`. Source patches and negative-control overlays are under `.tmp/followup-validation/`.

Final validation commands (through the wrapper with the toolchain/cache environment above):

```sh
./scripts/go-limited.sh test -race -count=3 -timeout=240s ./sdk/api/handlers/openai
./scripts/go-limited.sh test -race -count=1 \
  ./internal/api/... ./internal/logging ./internal/runtime/executor/... \
  ./sdk/api/handlers/... ./sdk/cliproxy/... \
  ./internal/translator/codex/claude ./internal/translator/openai/claude \
  ./internal/util ./internal/accountpolicy/... \
  ./internal/accountpolicybindings/... ./internal/accountpolicyledger/... \
  ./internal/accountpolicyusage/... ./internal/watcher/... ./test
./scripts/go-limited.sh test -count=1 ./...
./scripts/go-limited.sh build -o .tmp/followup-validation/server-check ./cmd/server
```

The management concurrency regression additionally ran 50 repetitions under the race detector. Verification binaries are removed after successful compilation.

## Remaining limits

There are no unresolved patch conflicts or failing final checks. The earlier failures in the original two-commit report and intermediate runs are retained as historical evidence; the final checks above include their fixes. No live-provider soak test or external CLIProxyAPIHome service validation was performed. Home interactions were verified with isolated local fixtures. No production deployment, restart, configuration change, or state migration is part of this branch.
