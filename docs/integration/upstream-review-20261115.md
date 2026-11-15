# Upstream review 2026-11-15 (upstream/main 43806c86, v8.0.73)

This review covers the 31 commits in `1aaf56bc..43806c86`, against production
`60ed2b2f`. Operator guidance: take only real benefits (security, routing,
performance, reliability on used paths), and nothing that could affect custom
account routing.

Evidence comes from `logs/main.log`, which covers 2026-11-14 14:30 to
2026-11-15 19:19. Over that window:

- 6755 × 200 on `/v1/messages`, 4183 × 200 on `/v1/responses`;
- 577 × 499 on `/v1/responses`, 8 × 499 on `/v1/messages`;
- 1 × 429 in total;
- 0 calls to `/v1/chat/completions`;
- about 17.5k authenticated management calls (account-policy dashboard,
  diagnostics, capabilities, accounts, plugin quota, auth-files).

## Security

No new advisories. The only published ones (GHSA-4w5v-m7vx-jpmh and
GHSA-j4xx-x47j-7h54) were already fixed by `7bcd5867`.

## Taken

| Upstream | Port | Why |
| --- | --- | --- |
| 51b98e74 (#7519) cache verified management secret hashes for 1 h | `8008ff62` | Production has no env management secret. Every management call runs bcrypt cost 10 against the config hash, about 17.5k calls in 29 h. The cache is bound to the active hash and invalidated on rotation; invalid keys are never cached; fail counting and IP bans are unchanged. Management auth only, no routing. Ships with the next approved deploy, not alone. |

The only conflict was in the `Handler` struct; the fork's account-policy fields
were kept. The v8, account-policy and redis-queue auth paths all use
`AuthenticateManagementKey`, so all of them get the cache.

## Skipped

- 4da9f685 (#7521), pool-cooldown Retry-After on the terminal 429. There was one
  429 in 29 h, and the commit touches `sdk/cliproxy/auth` selection. Added as a
  watch item.
- 43806c86, Claude `json_schema` format → `output_config.format`. It only affects
  the OpenAI chat/Responses → Claude translators, and no such traffic exists.
- 3d1a0e0d, strict provider scoping in `GetModelInfo`. The Gemini part is unused;
  the registry part changes model lookups that routing depends on, and there is
  no observed shadowing.
- 5b63e29a / 95ebe39c, OAuth plugin model scoping and v8 plugin credential
  identity. There is no observed problem with the OpenCode Go plugin, and the
  change rewrites credential synthesis (`internal/watcher/synthesizer`).
- 22d40703, 100f2de1, 6acfb76b, 05a353cf, 7904efd7, b285660f: SDK and plugin
  extension features, not needed.
- 905e70c4: new sensitive-pattern cloaking feature.
- Gemini, Antigravity, Devin, SIWC, Muse, wstunnel and docs changes: unused paths.

## Daily watch

The operator approved a daily automatic check at 04:00 Europe/Berlin. The
procedure, authority and protected paths are in `ops/upstream-watch/README.md`;
state is in `.tmp/upstream-watch/state.json`.
