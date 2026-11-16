# Upstream review 2026-11-16 (upstream/main 14144df5, v8.0.74)

Daily watch run per `ops/upstream-watch/README.md`. This review covers the 9 commits
in `43806c86..14144df5`, against production `60ed2b2f` (image df0280d3afe8, healthz
200 at run start).

## Evidence

`logs/main.log` covers 2026-11-14 14:30 to 2026-11-16 04:01 (about 37.5 h). From
the `gin_logger` status column:

- 9401 × 200 on `POST /v1/messages`, 5024 × 200 on `POST /v1/responses`;
- 779 × 499 on `/v1/responses` and 16 × 499 on `/v1/messages` (client cancels);
- 1 × 429 (`/v1/messages`, 2026-11-14 18:27:51), 1 × 408 (`/v1/responses`,
  2026-11-14 22:53:37), 1 × 400 (`/v1/messages`, 2026-11-15 00:19:32, during the
  60ed2b2f cutover);
- 4 × `GET /v1/responses` (Responses WebSocket), at 19:02, 20:52, 21:20 and
  00:19. All came from one client, ran 3.5–5.9 s and ended with `forward failed
  ... context canceled`. Three of them coincide with deploy verifications.
- 0 lines with `usage_limit_reached`, `model_cooldown`, `response.create` or
  `advisor`.

Request logs cover 2026-11-15 22:15 to 2026-11-16 04:02 (1454 files): all 1427
`API RESPONSE` sections are `Status: 200`, and no request carries an `advisor_*`
tool.

## Security

No new advisories. GHSA-4w5v-m7vx-jpmh and GHSA-j4xx-x47j-7h54 (both high,
published 2026-10-27) are already fixed in production by `7bcd5867`, an ancestor
of `60ed2b2f`.

## Taken

Nothing.

## Proposed (operator decision)

- a8c9c8a4 (#7585): fail over pre-commit Responses WebSocket `response.create`
  quota/auth errors. It unpins and excludes the failed credential, so it widens
  failover and does not pin. It touches the protected path `sdk/cliproxy/auth`
  (`conductor_execution.go`, `conductor_home.go`), and the WebSocket path carries
  only verifier probes with no failures. Recommendation: skip until Codex/pi
  clients use the WebSocket transport for real traffic. Watch item.

## Skipped

- 14144df5 (#7586): route duplex quota errors through the replay guard after
  `response.created` on the Responses WebSocket. No quota or cooldown errors and
  no real WebSocket traffic. It builds on a8c9c8a4's changes in
  `openai_responses_websocket_forward.go`. Watch item together with a8c9c8a4.
- 8bdffcad (#7591): strip thinking suffixes from the `advisor_*` tool `model`.
  No client sends an advisor tool, and there are no matching 400s. Watch item.
- 4a9effcb (#7587): Gemini→Claude translator text-block coalescing. Unused path.
- 508f8708, b4fdfdec: replace `encoding/json` and `regexp` with moego across the
  tree, touching `go.mod`. No benefit. Future ports will have to translate more
  moego (`ojson` and moego regexp) hunks.
- 6ddea6e1, f7bbd32d, b48c4387: README changes.

## Gates and review

No port, so no gates, no Codex review and no deploy.
