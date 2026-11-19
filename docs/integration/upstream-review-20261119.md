# Upstream review 2026-11-19 (upstream/main db7f177d, v8.0.78)

Daily watch run per `ops/upstream-watch/README.md`. This review covers the 11
commits in `a5e8f69e..db7f177d` (v8.0.78), against production `60ed2b2f`
(image df0280d3afe8, container started 2026-11-18T18:44:54Z, State `running`,
healthz 200 at run start). The previous deploy, interrupt release `7babfb7d22`,
was already final `deployed` and host-verified (interruption 10.187 s). No
`cliproxy-idle-*` unit was active.

## Model check

- Routable models: authenticated `GET /v1/models` (Bearer and Claude format)
  both 200. 16 `gpt-*` and 18 real `claude-*` IDs, identical to `known_models`.
  No new models, so no test requests were due.
- Codex picker catalog: `static/catalogs/codex-client-compat.json` was written
  2026-11-18 19:19, after the last official `codex_client_models.json` change
  (`router-for-me/models` 829b3b7f, 2026-11-17 22:16Z). It lists every routable
  non-image `gpt-*` model. The `cliproxy-catalog-compat` crontab block points at
  `.tmp/retained/cliproxy-catalog/refresh_catalog.py`, and its hourly runs log
  `unchanged` (latest 03:17). No repair needed. The newer models repo commit
  2dabf224 (2026-11-18 23:11Z) only adds Gemini 4 Neon XHigh (unused).
- Codex client: the codex-config runtime reports `codex-cli 0.170.0`. The highest
  catalog `minimal_client_version` is 0.166.0. No upgrade needed.
- Advisor: no newer Sol (`gpt-6.2-sol` is current) or Opus (`claude-opus-5-5` is
  current). No switch.

## Evidence

`logs/main.log` covers 2026-11-18 13:36 to 2026-11-19 04:02 (about 14.5 h). From
the `gin_logger` status column:

- 13902 × 200 on `POST /v1/messages`, 6901 × 200 on `POST /v1/responses`,
  1061 × 200 on `count_tokens`, 1 × 200 `GET /v1/responses` (Responses
  WebSocket, 19:45:19, during the 7babfb7d22 live verification; ended with a
  client `context canceled`);
- 206 × 499 on `/v1/responses` and 6 × 499 on `/v1/messages`: client cancels of
  0.3–8.0 s (median 2.0 s), none ≥ 85 s;
- 8 × 503 on `/v1/responses`:
  - 3 at 19:25:41–43: Codex `upstream connect error or disconnect/reset before
    headers` (upstream side, before the 19:44 deploy);
  - 5 fast ones (0.45–0.86 s) at 14:35:39, 14:35:40, 14:35:48, 14:48:42 and
    14:49:16, one tailnet client. They have no selector or conductor line, so
    they were rejected before credential selection. Each came within 1–35 s of a
    Codex `server_is_overloaded` event (below). Request logs for that time have
    rotated, so the cause is not established;
- 6 × 409 on `/v1/responses` at 19:47:17–25 (one client, 3 min after the 19:44
  cutover): the fork's `account_policy_owner_unknown` conversation-ownership
  rejection. None since;
- 3 × 429 on `/v1/messages` at 19:58:12, 19:59:10 and 20:50:27, each logged
  with `Anthropic rate limit window rejected; falling back to generic
  exponential backoff`;
- 2 × 401 at 19:14:07 on `GET /v1/models` from the Docker gateway (not a client).

Codex `server_is_overloaded` (`service_unavailable_error`, nested
`retry-after` 301 and 321) hit `gpt-6.2-astra` twice: request 4c6a7aa5
(conductor 502 at 14:35:38) and 342d0ea0 (14:48:41). Both failed over and the
client got 200 (14.7 s and 17.1 s). Conductor also logged single Claude events:
weekly-limit 429s (19:45:04, 00:37:43), one account rate-limit 429 (20:25:48) and
one Cloudflare 520 after 20 s (20:00:16).

0 `panic`, `non-whitespace thinking`, `bio_policy`,
`misalignment_policy_violation`, `content_policy_violation`,
`Refresh token expired`, `invalid_grant`, `advisor_`, `unexpected EOF`,
`connection reset by peer` or `/interactions` lines.

Selector lines show only native routes: `claude-opus-5-5` (11807),
`gpt-6.2-sol` (3475), `gpt-6.1-sol` (2034), `claude-fable-5-2` (1613),
`claude-opus-5` (1073), `gpt-6.2-astra` (804), `gpt-6.1-terra` (339),
`claude-sonnet-4-6` (299), `gpt-6-luna` (249), `claude-sonnet-5-5` (178).

Request logs cover 2026-11-19 03:20 to 04:03 (516 files). Their exact
`Status:` lines are 520 × 200 for API RESPONSE sections and 520 × 200 for the
client RESPONSE.

Container CPU at run time: 0.11 %, 317 MiB. No spin.

## Security

No new advisories. GHSA-4w5v-m7vx-jpmh and GHSA-j4xx-x47j-7h54 stay fixed by
`7bcd5867`, an ancestor of the served commit.

## Taken

Nothing. No commit is class A, B or M.

## Skipped

- db7f177d: Codex session-scoped instruction baselines. An opt-in feature: it
  only activates when a client sends the CPA-specific
  `client_metadata.cpa_session_instructions` marker (`start`/`reuse`), which
  stock Codex and pi do not send. It also needs `internal/kvstore`, which the
  fork lacks, and rewrites the Codex WebSocket execute/stream path. No benefit
  for production clients.
- 56ab1549: Interactions→Codex system-instruction conversion. No
  `/interactions` traffic.
- 9d60b36f, 65864c04: Interactions↔Gemini stream translators. Gemini unused.
- 9b4f3f30: Gemini→Claude tool schema translator. No Gemini-format clients.
- 88ef67a1: Gemini 4 Neon XHigh in the embedded registry (`internal/registry`,
  protected, data-only, unused).
- bd2e0a98 (#7655): `Kind` field on `pluginapi.ResponseInterceptRequest`. A
  plugin API feature for response interceptors. The OpenCode Go plugin is a
  provider, not an interceptor.
- 76bae1fe: pluginhost test-only change.
- d0ee9ca7: makes the Claude OAuth `org:create_api_key` scope configurable.
  Feature, login-time only, touches `internal/auth`, `internal/config` and
  `config.example.yaml` (protected). Added as a watch item.
- 61a4c5d0: config v8 migration warning cosmetics (`internal/config`,
  protected).
- 5002fe72: defer Claude OAuth refresh during macOS maintenance wakes. Darwin
  only (production is Linux), touches `sdk/cliproxy/auth` and `internal/auth`
  (protected).

## Watch items reviewed

- deacbf21 + 7dd360d9 (Codex nested Retry-After): the trigger text matched. Codex
  overload errors with nested `retry-after` occurred twice. They did not reach
  clients, because failover served both with 200. The fix feeds that hint into
  `retryAfterFromError`, which the fork's own Codex capacity recovery
  (`sdk/cliproxy/auth/codex_capacity_recovery.go`) and cooldown scheduling
  consume. A 301–321 s hint would change cooldown and capacity-retry timing, so
  this is routing-adjacent in effect. Not class B and not auto-deployable. It is
  now propose-only, and not recommended without a design review of its
  interaction with capacity recovery.
- 4da9f685: 3 terminal 429s on `/v1/messages` in 14.5 h, isolated, no pool-wide
  cooling. Not triggered.
- 2c5b01f4: 0 resets or EOFs on Claude, 499s ≤ 8.0 s. Not triggered.
- a8c9c8a4 + 14144df5: one Responses WebSocket session (deploy verification),
  no 429/401 on `response.create`. Not triggered.
- 69637a66, a5795047, 8bdffcad, 1f76e479 group, 0bfb3d01, e1384430 group: 0
  matches.
- 7df56074: no CPU spin.

## Gates and review

No port, so no gates, no Codex review and no deploy. `447a355d` (release
c70877e59c, stale against the 2026-11-18 config; must be re-prepared) still
waits for the operator's go-ahead. `51b98e74` (port 8008ff62) still rides with
the next approved deploy.
