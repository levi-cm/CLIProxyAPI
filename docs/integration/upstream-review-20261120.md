# Upstream review 2026-11-20 (upstream/main 6faa8d20, v8.0.79)

Daily watch run per `ops/upstream-watch/README.md`. This review covers the 5
commits in `db7f177d..6faa8d20` (v8.0.79, published 2026-11-19 13:11Z), against
production `60ed2b2f` (image df0280d3afe8, container started
2026-11-18T18:44:54Z, State `running`, healthz 200 at run start). No
`cliproxy-idle-*` unit was active and no release was waiting. The last deploy,
interrupt release `7babfb7d22`, was already final `deployed`. The stale release
`c70877e59c` (447a355d, pending_unapproved) was not touched.

## Model check

- Routable models: authenticated `GET /v1/models` (Bearer and Claude format)
  both 200. 16 `gpt-*` and 18 real `claude-*` IDs, identical to `known_models`.
  No new models, so no test requests were due.
- Codex picker catalog: `static/catalogs/codex-client-compat.json` (written
  2026-11-18 19:19) is newer than the last official `codex_client_models.json`
  change (`router-for-me/models` 829b3b7f, 2026-11-17 22:16Z) and lists every
  routable non-image `gpt-*` model. The `cliproxy-catalog-compat` crontab block
  points at `.tmp/retained/cliproxy-catalog/refresh_catalog.py`; hourly runs log
  `unchanged` (latest 03:17, sha256 ea1f152a…). No repair needed.
- Codex client: `codex-cli 0.170.0`; highest catalog `minimal_client_version`
  is 0.166.0. No upgrade needed.
- Advisor: no newer Sol (`gpt-6.2-sol` current) or Opus (`claude-opus-5-5`
  current). No switch.

## Advisories

No new advisories. GHSA-4w5v-m7vx-jpmh and GHSA-j4xx-x47j-7h54 (2026-10-27) stay
fixed by `7bcd5867`, an ancestor of the served commit.

## Evidence

`logs/main.log` covers 2026-11-19 09:24 to 2026-11-20 04:02 (about 18.6 h).
`gin_logger` status column, client paths:

- 7841 × 200 `POST /v1/responses`, 6471 × 200 `POST /v1/messages`,
  361 × 200 `count_tokens`, 1298 × 200 `GET /v1/models`;
- 200 × 499 on `/v1/responses`, 3 × 499 on `/v1/messages` (client cancels);
- 8 × 429 on `/v1/messages`: one at 19:28:31 (100.121.114.101) and seven at
  21:31:11–21:33:32 from one client (100.97.146.120). The conductor shows
  `claude-fable-5-2` rate_limit_error ("This request would exceed your a…") on a
  single Claude account, retried within each request. Short burst, not the
  whole-pool cooling pattern of watch item 4da9f685;
- 699 × 404 `HEAD /claude-code/api/hello` and 238 × 404 `HEAD /api/hello`:
  Claude Code connectivity probes, not API traffic;
- 0 × 5xx, 0 × 409, 0 × 400 on any path.

0 `panic`, `non-whitespace thinking`, `bio_policy`,
`misalignment_policy_violation`, `content_policy_violation`,
`Refresh token expired`, `invalid_grant`, `advisor_`, `unexpected EOF`,
`connection reset by peer`, `server_is_overloaded`,
`account_policy_owner_unknown`, `usage_limit_reached`, `model_cooldown`,
`org:create_api_key` or `invalid_json_schema` lines. Two `/v1/responses` error
logs (03:42:18) are 408 `request_timeout` stream disconnects after a 200.

Request logs: 517 `v1-responses-*` files (about 1 h); 0 contain `"uniqueItems"`.

## Commits

| Commit | Change | Decision |
| --- | --- | --- |
| 6faa8d20 | Codex: strip `uniqueItems` from tool parameter schemas (#7690: gpt-6.2-astra 400 `invalid_json_schema`) | Skip, watch. Unprotected paths (`internal/client/codex/tool-schema`, executor `helps`), and the fork lacks the strip. Not class B: 0 × 400 and 0 `invalid_json_schema` in 18.6 h, and no current Responses request carries `uniqueItems`. Not class M: gpt-6.2-astra is not a new model and its routing works (804 selector routes yesterday). Trivial map-based hunk, no ojson translation needed if ever taken. |
| d0c90bb5 | Antigravity model probe: force HTTP/1.1 | Skip: Antigravity unused; `sdk/auth` protected. |
| edab7750 | Home-backed Codex continuity | Skip: Home unused; `sdk/cliproxy/auth` protected. |
| eff9e4c3 | Merge of #7693 (edab7750) | Skip. |
| f2b00303 | Antigravity Hub changelists 2.33.0 | Skip: Antigravity unused. |

## Taken

Nothing. No gates or reviews ran because no port was made. No deploy.

## Watch items

Existing items unchanged; none triggered. New item: 6faa8d20, trigger
`invalid_json_schema` / `Unimplemented keys: ["uniqueItems"]` 400s on
`/v1/responses` (e.g. pi/Zod/MCP tools on gpt-6.2-*).
