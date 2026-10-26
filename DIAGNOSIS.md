# Live Codex routing diagnosis — 2026-10-26

Production image: `cliproxyapi-fork:renewal-b8a73830`, serving source `b8a7383063021044f950da735597a0fc38ff5aa3`.
The serving container started at `2026-10-25T21:04:27Z`; it still had zero restarts when the investigation finished. Production configuration, credentials, policy journals, ownership records, and refills were not edited. Normal diagnostic inference requests were sent through the running router; its normal runtime persisted their usage/ownership.

## Confirmed upstream capacity rejection and local retry defects

Recent retained error logs show `POST https://chatgpt.com/backend-api/codex/responses`, model `gpt-6.1-sol`, failing with HTTP 429 and this exact upstream body:

```json
{"error":{"code":"turn_rate_limit_exceeded","message":"Codex is at capacity, please try again later","type":"rate_limit_error"}}
```

Both Codex accounts have returned it while ordinary weekly allowance remained available. This is distinct from the provider's `usage_limit_reached` exhaustion response. Do not infer an IP ban or exhausted allowance from this body.

Three source-level mechanisms were verified against the serving source:

1. `isCodexModelCapacityError` matches messages mentioning a *model* at capacity, but does not recognize the observed `turn_rate_limit_exceeded` code or “Codex is at capacity” text.
2. A fixture HTTP upstream returning this body and `Retry-After: 120` loses the retry hint in both `Execute` and `ExecuteStream`: the resulting `statusErr.RetryAfter()` is nil.
3. `Manager.MarkResult` classifies the transient 429 as `Quota.Exceeded=true`, `Reason="quota"`, initially with approximately one second of cooldown.

The live retry configuration is three retries and maximum retry interval 30 seconds. Merely raising a retry count cannot correct the lost header or the classification. A longer maximum retry interval is relevant only after an actual upstream wait hint is preserved; long established requests must remain cancellation-aware without adding a transport timeout.

The existing error-only logs contain upstream bodies but did not retain the upstream response headers. Consequently this investigation does **not** claim a particular Retry-After duration for these local failures. Header loss was reproduced with a controlled fixture, not inferred from absent log lines.

## Confirmed fresh-chat ownership false positive

`policyRequestHasLocalState` recursively scans every request object, including tool declarations and JSON schemas. It treats the presence of keys such as `conversation_id`, `previous_response_id`, `encrypted_content`, or `turn_state` as account-specific state even when those keys merely name function parameters.

A controlled production comparison through `100.82.251.30:8317/v1/responses` used fresh session IDs, `gpt-6.1-sol`, plain input, and one tool declaration:

| Only changed input | Result |
| --- | --- |
| Tool parameter property named `conversation_id` | HTTP 409, `account_policy_owner_unknown`, approximately 0.01 seconds |
| Same tool parameter named `lookup_id` | HTTP 200, completed response, approximately 2.55 seconds |

There was no actual encrypted continuation or previous response reference in either request. Fixture tests reproduce the false positive for all four property names above. This is a confirmed router bug capable of breaking new chats independently of account switching or CLI version.

**Attribution limit:** the original bodies of the remote client's earlier 409 requests are no longer retained. The main log records their status and timing, but not the offending JSON path. The controlled reproducer does not prove that every user-reported 409 came from tool schemas. Genuine missing caller/session/model ownership remains a separate possible cause, particularly for old or resumed conversations.

## Ownership, routing and the latest merge

- The sampled main log contained 12 HTTP 409 responses from the affected remote client at 17:05–17:08 local time, before this investigation's deliberate negative probes. These early rejections have no upstream execution log entry.
- Durable ownership was readable from inside the serving container and grew during ordinary traffic. No `conversation owner was not persisted` error appeared in the sampled log.
- Fresh inference followed by genuine encrypted continuation succeeded across Tailscale, loopback and LAN with the same observed owner. Two WebSocket turns and HTTP streaming also completed.
- Isolated fixture tests prove that changing the preferred deadline account does not move an established encrypted continuation. A known owner in cooldown returns `unsafe_affinity_unavailable`, rather than the reported `account_policy_owner_unknown`; these errors must not be conflated.
- Retained failed encrypted retries preserved their upstream Session-Id and Thread-Id. One retained request tried both accounts, but it contained no encrypted state. This is not evidence of unsafe encrypted migration.
- Routine affinity log session values are truncated to eight characters; Codex-prefixed UUID sessions can collide in this display. These truncated values cannot establish that a single real conversation switched accounts.
- Live policy is enabled in `earliest_deadline` mode, with `deadline_at_boundary` affinity and no active routing override. The accounts' ordinary weekly reset deadlines were over seven hours apart, outside the four-hour automatic-spreading gate. No `automatic_concurrent_pressure_forecast` reason appeared in the 200 sampled decisions.
- At the sampled quota observation the earlier-renewing account was 13% used and the other 0% used. Expiring saved reset credits can also contribute an earlier effective deadline; the mandatory expiry guard remains authoritative.
- The local-state detector is byte-for-byte identical between prior deployed source `2896a388` and current source `b8a73830`. Durable-binding and session-identity files are also unchanged between these versions. The observed defects are therefore not newly introduced in those functions by the latest merge. Other changes could affect exposure; that was not established.

## Outbound IP

No global request proxy, per-Codex-credential proxy, or container proxy environment variable is configured. There is no Tailscale exit node. The host routes ordinary Internet traffic through `192.168.178.1` on `eno1`; the container has an ordinary bridge default route. The host public egress address was checked independently. This supports direct egress through the Mini PC's home gateway, not the connecting client's public IP. A separate container public-IP probe was unavailable because the minimal image lacks the requested utility; it is not represented as a successful measurement.

## Upstream investigation

As checked during this run:

- Latest release: [v8.0.44](https://github.com/router-for-me/CLIProxyAPI/releases/tag/v8.0.44).
- Exact matching open report: [issue #7017](https://github.com/router-for-me/CLIProxyAPI/issues/7017). It describes the same error on an older official v8.0.37 installation and reports no matching fix through current main. This independently argues against attributing the upstream capacity response solely to the local merge.
- [PR #3229](https://github.com/router-for-me/CLIProxyAPI/pull/3229) is unmerged. Its capacity predicate does not match the new error code/message; it is not a complete ready-to-apply fix for this incident.
- Current upstream `codex_executor_terminal.go` was fetched and checked directly: its capacity classifier still does not recognize `turn_rate_limit_exceeded` or the exact observed message, and its body retry parser does not consume HTTP Retry-After headers.
- The earlier fixed [issue #5634](https://github.com/router-for-me/CLIProxyAPI/issues/5634) concerns the older in-stream “Selected model is at capacity” response. That wording is already recognized locally and differs from this incident.
- [OpenAI status](https://status.openai.com/) displayed operational status when checked. Aggregate status does not rule out model- or account-specific admission failures.

The upstream report supplies context, not proof of the exact local Retry-After value or the upstream reason for selecting these accounts for rejection.

## Recommended correction

1. Restrict ownership-state detection to the actual request continuation fields and typed response input items. Exclude tool/function definitions, JSON schemas and unrelated metadata. Keep rejection of genuinely unowned encrypted reasoning, compaction items and previous-response references.
2. Explicitly classify `turn_rate_limit_exceeded` as transient admission/capacity pressure. Preserve valid Retry-After hints through HTTP, streaming and WebSocket error paths and client responses. Use transient admission state instead of marking genuine allowance exhausted. Retain the verified conversation owner during backoff and respect cancellation and configured retry limits.
3. Keep earliest-deadline routing and mandatory refills. Changing global round-robin/fill-first labels or forcing a single account does not fix either parser defect. Existing bound conversations still need their verified account; do not move their encrypted state for balancing.
4. Add sanitized rejection diagnostics containing the classification, JSON path category, binding lookup outcome and hashed session/model identity. Never log prompts, encrypted contents, inference keys or OAuth tokens. Use this to attribute any remaining new-chat 409 to an exact path rather than assume all such errors have one cause.
5. Stage any implementation and tests in isolation. Production replacement remains blocked unless zero-downtime handoff, draining, ownership/state preservation and rollback are verified under AGENTS.md. No replacement was attempted.

## Verification artifacts

- `sdk/cliproxy/auth/diagnosis_owner_test.go`
- `internal/runtime/executor/diagnosis_turn_capacity_test.go`
- Sanitized run output: `/tmp/cliproxy-route-check-q267vj7i/reproduction-results.log`

Focused existing binding restart/isolation, credential quota scope and bootstrap-capacity tests passed before the new reproductions. The diagnostic tests deliberately fail for the confirmed defects and pass for retained-owner safety. The required server compilation passed using `scripts/go-limited.sh` with Go 1.26.10; its temporary binary was removed. No fix is implemented by these diagnostic tests.

Final availability checks returned HTTP 200 on loopback, LAN and Tailscale. Mandatory automation remained enabled, `auto_expiring`, writable, and permanently required. Serving start time and zero restart count were unchanged.
