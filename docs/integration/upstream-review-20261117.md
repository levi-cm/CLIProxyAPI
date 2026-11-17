# Upstream review 2026-11-17 (upstream/main 7df56074, v8.0.76)

Daily watch run per `ops/upstream-watch/README.md`. This review covers the 16
non-merge commits in `14144df5..7df56074` (v8.0.75, v8.0.76), against production
`60ed2b2f` (image df0280d3afe8, State `running`, healthz 200 at run start).

## Evidence

`logs/main.log` now covers only 2026-11-16 16:49 to 2026-11-17 04:01 (about
11 h; the file was rotated). From the `gin_logger` status column:

- 9663 × 200 on `POST /v1/messages`, 4083 × 200 on `POST /v1/responses`,
  385 × 200 on `count_tokens`;
- 352 × 499 on `/v1/responses` and 48 × 499 on `/v1/messages`, all client cancels
  of 0.2–8.7 s (no ~90 s zero-byte stalls);
- 5 × 400 on `/v1/responses` (17:54:17, 17:55:18, 17:56:18, 17:56:28, 18:27:16;
  33.7 s–1m11s) and 1 × 400 on `/v1/messages` (20:57:33), all from one client.
  None has an `upstream execution failed` line, which `warnLogUpstreamFailure`
  suppresses only for `isRequestInvalidError`: production already classified
  them as request faults (no cooldown, failover stopped). Their request logs have
  rotated out;
- 0 × 429, 0 × 5xx in the status column; 0 refresh log lines; 0 `panic`,
  `usage_limit_reached`, `model_cooldown`, `advisor_`, `response.create`, and no
  `GET /v1/responses` (Responses WebSocket);
- 3 × `upstream execution failed`, all Claude `read: connection reset by peer`
  on one account: 21:44:13 (6.7 s), 03:44:59 (17.5 s) and 03:45:00 (46.4 s). The
  last two hit two different sockets in the same second, after `API RESPONSE 1
  Status: 200` and streamed content; the client got a mid-stream 500.

Request logs cover 2026-11-17 02:59 to 04:02 (1347 files): API RESPONSE /
RESPONSE statuses are 1342 × 200, 4 × 499 and the 2 × 500 above. Hits for
`bio_policy`, `usage_limit_reached` and `thread_not_found` are all inside request
bodies or this run's own 200 model output.

Container CPU: 9503 CPU-seconds over 51.7 h of uptime (about 5 % of one core).

## Security

No new advisories. GHSA-4w5v-m7vx-jpmh and GHSA-j4xx-x47j-7h54 stay fixed by
`7bcd5867`.

## Taken

Nothing. No commit is class A or class B.

## Proposed (operator decision; protected paths)

- 0bfb3d01 (#7596): treat Anthropic `invalid_grant` "Refresh token expired" as a
  terminal refresh error (`RequiresSignIn`, stop retrying).
  `sdk/cliproxy/auth/conductor_cooldown.go`, 5 lines. The fork knows only the
  `refresh_token_expired` code. No refresh failures in the window.
  Recommendation: skip until a Claude refresh fails with that text. Watch item.
- 7df56074: back off non-refreshing auths whose next check is already past
  (`auto_refresh_loop.go`, 7 lines). Production has the same code shape, but CPU
  shows no spin. Recommendation: skip. Watch item.

## Skipped

- 1f76e479, 20df0c25, 80e8350c, 5a373ede: Codex/Responses policy-block and
  string-error classification (bio_policy, misalignment, content policy → 400;
  fallback messages; SSE status). These are large ojson rewrites of
  `clienterror`, the Codex terminal and WebSocket errors and the Responses
  handlers, and 1f76e479 also touches a `sdk/cliproxy/auth` test. There were no
  policy-block failures, and the six 400s were already handled as request faults.
  Watch item.
- 7dd360d9, deacbf21 (#7600): preserve (nested) Retry-After on Codex overload and
  inferred WebSocket statuses. No Codex 429/503 in the window. Watch item.
- b4410dd5: payload rules as final barrier (Claude count_tokens, Codex WS duplex,
  xAI). Production `config.yaml` has no `payload:` section, so this has no effect.
- f00a3d39, 5aa44948, ecae1a4f: new payload user-text replacement feature
  (`internal/config`, `config.example.yaml`). Feature; protected.
- 7fcdfcfe: Codex scheduler continuity plugin hook (+6.8k lines, heavy
  `sdk/cliproxy/auth` changes). Feature; protected.
- b1faf0bd: `thread_not_found` for pinned Claude Thread owners. The fork has no
  Claude Thread conductor, and the change pins credential selection.
- 240a0271: moego bump (`go.mod`); the fork does not use moego.
- 72f7b6b8: pluginhost test only.

## Watch items reviewed

- 2c5b01f4 (disable Claude connection reuse): not triggered. #7357 is about
  stale idle connections (zero-byte ~91 s stalls or 0.1 s EOF on the first request
  after idle). The three resets here came mid-stream after a 200, which disabling
  reuse would not prevent. Keep watching. A cluster of simultaneous resets on one
  account points upstream.
- 4da9f685, a8c9c8a4 + 14144df5, 8bdffcad, 69637a66, a5795047: not triggered.

## Gates and review

No port, so no gates, no Codex review and no deploy. No `cliproxy-idle-*` unit
was active. `447a355d` (release c70877e59c) still waits for operator go-ahead;
`51b98e74` (port 8008ff62) still rides with the next approved deploy.
