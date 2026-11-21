# Upstream review 2026-11-21 (upstream/main f5c5681a, v8.0.80)

Daily watch run per `ops/upstream-watch/README.md`. This review covers the 28
commits in `6faa8d20..f5c5681a` (v8.0.80, published 2026-11-20 14:54Z), against
production `60ed2b2f` (image df0280d3afe8 `cli-proxyapi-fork:trend-advice-60ed2b2f`,
container started 2026-11-18T18:44:54Z, State `running`, healthz 200 at run
start). No `cliproxy-idle-*` unit was active and no release was waiting. The last
deploy, interrupt release `7babfb7d22`, was already final `deployed`. The stale
release `c70877e59c` (447a355d, pending_unapproved) was not touched.

## Model check

- Routable models: authenticated `GET /v1/models` (Bearer and Claude format)
  both 200. 16 `gpt-*` and 18 real `claude-*` IDs, identical to `known_models`.
  No new models, so no test requests were due.
- Codex picker catalog: `static/catalogs/codex-client-compat.json` (written
  2026-11-18 19:19, 14 models) is newer than the last official
  `codex_client_models.json` change (`router-for-me/models` 829b3b7f,
  2026-11-17 22:16Z) and lists every routable non-image `gpt-*` model. Newer
  `router-for-me/models` commits (2dabf224, 695ecc9f) touch Gemini only. The
  `cliproxy-catalog-compat` crontab block points at
  `.tmp/retained/cliproxy-catalog/refresh_catalog.py`; hourly runs log
  `unchanged` (latest 03:17, sha256 ea1f152a…). No repair needed.
- Codex client: `codex-cli 0.170.0`; highest catalog `minimal_client_version`
  is 0.166.0. No upgrade needed.
- Advisor: no newer Sol (`gpt-6.2-sol` current) or Opus (`claude-opus-5-5`
  current). No switch.

## Advisories

No new advisories. GHSA-4w5v-m7vx-jpmh and GHSA-j4xx-x47j-7h54 (2026-10-27) stay
fixed by `7bcd5867`, an ancestor of the served commit.

## Evidence

`logs/main.log` covers only 2026-11-20 21:08 to 2026-11-21 04:01 (about 7 h; it
rotated). `gin_logger` status column, client paths:

- 838 × 2xx `POST /v1/responses`, 2778 × 2xx `POST /v1/messages`,
  117 × 2xx `count_tokens`;
- 11 × 499 on `/v1/responses`, 46 × 499 on `/v1/messages` (client cancels);
- 31 × 503 on `/v1/responses`, 21:09:32–21:15:40. Upstream Codex outage: 10
  conductor lines `upstream connect error or disconnect/reset before headers …
  remote connection failure` on both Codex accounts (21:09:31–21:15:02), models
  gpt-6.2-sol and gpt-6.1-sol. 15 of the 503s (21:10:56–21:11:05, one client,
  2–5 ms, no conductor line) are fast pre-selection 503s while the pool was
  failing, the same pattern as 2026-11-19. Not an upstream-fix target;
- 4 × 409 on `/v1/responses` at 21:10:32–21:10:34: the fork's own
  `account_policy_owner_conflict` guard for one gpt-6.2-sol conversation during
  that outage. Intended behaviour;
- 1 × 400 `/v1/messages` at 22:38:10 (58 ms, no conductor line; the request log
  has rotated, so the cause is unknown).

0 `panic`, `non-whitespace thinking`, `bio_policy`,
`misalignment_policy_violation`, `content_policy_violation`,
`Refresh token expired`, `invalid_grant`, `unexpected EOF`,
`connection reset by peer`, `server_is_overloaded`,
`account_policy_owner_unknown`, `usage_limit_reached`, `model_cooldown`,
`rate_limit_error`, `org:create_api_key`, `invalid_json_schema` or `uniqueItems`
lines.

Request logs (391 `v1-responses-*`, 951 `v1-messages-*`, about 3.5 h):

- 0 client requests carry a `Content-Encoding` header (the gzip hits are all in
  upstream API RESPONSE headers);
- 0 carry advisor blocks (`advisor_tool_result`, `"name":"advisor"`);
- 0 contain `response.refusal.delta`;
- 0 Responses requests contain `"uniqueItems"`;
- 88 `/v1/messages` logs are cross-format `gpt-*` traffic.

## Commits

| Commit | Change | Decision |
| --- | --- | --- |
| 91a3f455 | Claude: strip foreign advisor blocks after credential rebind (#7638) | Skip, watch. Unprotected (`claude_executor*`, executor `helps`), large ojson rewrite. Not class B: 0 advisor blocks in request logs, 0 advisor errors in main.log. Pairs with watch item 8bdffcad. |
| 6e7a1b29 | Decode gzip request bodies on Claude routes (#7711) | Skip. Feature, not a security fix (adds gzip decoding under the existing cap). 0 client requests use `Content-Encoding`. |
| 5f7c0553 | Codex client catalog: baseline model synthesis, fallback `minimal_client_version` normalization (#7697) | Skip. The compat file lists every routable model, so the fallback builder is unused; the rest is `cmd/fetch_*` and embedded registry data (protected, data-only). |
| ca459712, e442c32d, 2e940ba1 | Meta executor Responses synthesis; refusal parts in Codex response translators | Skip. Meta provider unused; 0 refusal deltas in cross-format traffic. Translator-only. |
| 3a0d3023 | Anthropic stop_reason completion classification (#7722) | Propose only (protected `sdk/cliproxy/auth`). No observed symptom. |
| 3ad507bf | Cancellation and Retry-After cooldown consistency (#7720) | Propose only (protected). Stops plain client cancels being recorded as credential failures; 57 × 499 last night, no evidence that cancels cooled credentials. Watch. |
| 2dbad4dd, 0a34244e, 2f57a5a6, d4f6edb1, 0399de38, f5c5681a, d52047a6, dfdb3ba4 | Conductor auth prep, refresh locks, 401-retry metadata, bootstrap Retry-After scoping | Propose only (protected `sdk/cliproxy/auth`, `internal/auth`). 0 upstream 401s, 0 refresh failures. |
| 84e0cd9b, 9be2d5f1 | Home execution parity / empty stream | Skip: Home unused; protected. |
| 5bfae629 | Plugin credential-scoped after-auth termination | Skip: feature; protected. |
| a35e8be3, 0861f9e1, 99a1c1ca, f8e297aa, 3a24d03c, bba22ed9 | Antigravity | Skip: Antigravity unused (several protected). |
| c1040cc0 | Gemini 4 max tokens (embedded registry) | Skip: Gemini unused; data-only, protected. |
| f19d4a6f, 0c292388 | Plugin example deps, sponsor docs | Skip. |

## Taken

Nothing. No gates or reviews ran because no port was made. No deploy.

## Watch items

Existing items unchanged; none triggered. New items:

- 91a3f455: 400s on `/v1/messages` about advisor `tool_use_id`/`advisor_tool_result`
  after a credential switch (evaluate with 8bdffcad; unprotected).
- 3ad507bf: credentials cooling or recorded as failed right after client
  cancellations (499); propose only (protected).
