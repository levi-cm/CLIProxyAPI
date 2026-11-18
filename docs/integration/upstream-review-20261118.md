# Upstream review 2026-11-18 (upstream/main a5e8f69e, v8.0.77)

Daily watch run per `ops/upstream-watch/README.md`. This review covers the 13
non-merge commits in `7df56074..a5e8f69e` (v8.0.77 plus a5e8f69e), against
production `60ed2b2f` (image df0280d3afe8, State `running`, healthz 200 at run
start).

## Evidence

`logs/main.log` covers 2026-11-17 14:44 to 2026-11-18 04:01 (about 13 h). From
the `gin_logger` status column:

- 10035 + 363 × 200 on `POST /v1/messages`, 2729 × 200 on `POST /v1/responses`,
  403 × 200 on `count_tokens`;
- 52 × 499 on `/v1/responses` and 1 × 499 on `/v1/messages`, all client cancels
  of 1.8–7.8 s (no ~90 s zero-byte stalls);
- 2 × 400 on `/v1/responses` at 23:13:10 (376 ms and 60 ms, one tailnet client),
  with no other log line for either request ID (request faults; their request
  logs have rotated out);
- 0 × 429 and 0 × 5xx; 0 `upstream execution failed`, `connection reset`, `EOF`,
  `panic`, refresh, `invalid_grant`, `usage_limit_reached`, `model_cooldown`,
  `cooldown`, `advisor_`, `non-whitespace`, `bio_policy`, `policy_violation`,
  `Retry-After`, `document`, `pdf` or `input_file` lines; no `GET /v1/responses`
  (Responses WebSocket) and no `/v1/chat/completions`;
- 363 × 404 `HEAD /claude-code/api/hello` and 53 × 404 `HEAD /api/hello`:
  client connectivity probes, unrelated to any upstream change.

Selector lines show only native routes: `claude-opus-5-5` (10661),
`gpt-6.1-sol` (2783), `claude-sonnet-5-5` (157), `claude-sonnet-4-6` (9) and
`claude-haiku-5-5` (2). Request bodies confirm `/v1/messages` carries only
Claude models and `/v1/responses` only `gpt-6.1-sol`. No request crossed the
Claude→Codex/OpenAI, Responses→Claude/OpenAI or Gemini translators. The OpenCode
Go plugin was polled for quota but routed no requests.

Request logs cover 2026-11-18 02:59 to 04:01 (838 files). Their exact `Status:`
lines are 1682 × 200, 2 × 499 and 1 × 401. The 401 was API RESPONSE 1 from
Anthropic at 03:05:39 on one Claude account. Failover served API RESPONSE 2 with
200 from another account, and the client got 200.

Container CPU: 17287 CPU-seconds over 75.7 h of uptime (about 6.3 % of one
core). The 24 h average since the last run is about 9 %, and a 12 s cgroup
sample also read 9 %. No spin.

## Security

No new advisories. GHSA-4w5v-m7vx-jpmh and GHSA-j4xx-x47j-7h54 stay fixed by
`7bcd5867`.

## Taken

Nothing. No commit is class A or class B.

## Skipped

- a5e8f69e: GPT-6.2 Astra and Sol in the embedded registry
  (`internal/registry`, protected), plus the model fetcher's Codex client version
  bump to 0.169.0. A deploy would not help. The official remote catalog
  (`router-for-me/models` 829b3b7f, 2026-11-17 22:16Z) already lists
  `gpt-6.2-astra` and `gpt-6.2-sol`. Production re-registered its Codex models
  after a catalog change at 2026-11-18 00:19:35, which overrides the embedded
  file anyway. The Codex client compat catalog
  (`static/catalogs/codex-client-compat.json`, frozen since 2026-10-27 because the
  `cliproxy-catalog-compat` cron points at a moved helper) still lacks both
  models. That is an operator item, not an upstream port.
- cc1d5d78, 25501b6b, 9f6672f5: payload replace features (regex, system and
  developer roles, path targets). They touch `internal/config` and
  `config.example.yaml`, both protected. Production `config.yaml` has no
  `payload:` section.
- 34d8fe0f (#7618, #7621): relays Gemini function-response media (an unused
  input format) and preserves case-only OAuth model aliases. It touches
  `sdk/cliproxy/auth`, `internal/config` and `internal/watcher`, all protected.
  Production has no `oauth-model-alias:`.
- e1384430 (#7617), e21cf8a5 (#7620): Claude document, PDF and tool_result media
  in the cross-format translators (codex/claude, openai/claude, Antigravity,
  Gemini, Interactions). No cross-format traffic. Large ojson hunks. Watch item.
- f08cb931 (#7619): PDF media type inference and tool-output media in the
  Responses→Claude and Responses→OpenAI translators. No cross-format traffic.
  Watch item, with the two above.
- e3d19b24, 3b5de44b: nested `input_audio` for OpenAI Chat→Codex. No Chat
  Completions traffic.
- 1c0ec3ce: request-log diagnostics. Local read, dial and stream errors move out
  of the API RESPONSE sections into the downstream RESPONSE section. Logging
  only. If it is ever taken, this watch's evidence method has to follow it.
- f7b3ccdf (#7630), 687caf78: pluginhost usage features (cache-write TTL token
  counts, optional executor usage attribution). Features.
- 51e60958, 193f04f7, 593df327, 4b028c84: merges.

## Watch items reviewed

None was triggered:

- 2c5b01f4: 0 resets or EOFs, and 499s ≤ 7.8 s.
- 4da9f685: 0 × 429.
- 69637a66: 0 matches.
- a5795047: 0 panics.
- a8c9c8a4 + 14144df5: no Responses WebSocket traffic.
- 8bdffcad: 0 `advisor_`.
- 1f76e479 group: 0 policy blocks and 0 × 5xx.
- deacbf21: 0 Codex 429/503.
- 0bfb3d01: 0 refresh failures.
- 7df56074: CPU about 9 % of one core, no spin.

## Gates and review

No port, so no gates, no Codex review and no deploy. No `cliproxy-idle-*` unit
was active and no idle-deploy lock was held. `447a355d` (release c70877e59c,
continuity preflight only) still waits for the operator's go-ahead. `51b98e74`
(port 8008ff62) still rides with the next approved deploy.
