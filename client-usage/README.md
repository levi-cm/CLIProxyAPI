# Codex account usage tool

This optional read-only integration keeps Codex CLI and upstream inference
interfaces unchanged. It does **not** fill the built-in `/status` Limits row:
Codex v0.160.0 uses full ChatGPT account-usage reads for that display, not rolling
quota headers from a custom model provider.
See the [pinned Codex status-data handling](https://github.com/openai/codex/blob/rust-v0.160.0/codex-rs/tui/src/chatwidget/rate_limits.rs)
and [official MCP configuration](https://developers.openai.com/codex/mcp).

Ask Codex: **"Use proxy_account_usage to show the account used for this session,
weekly percentage used and remaining, and the seven-day refresh time."**

The tool requires the exact thread UUID (`CODEX_THREAD_ID` in the current
execution environment, or Session in `/status`). Never guess it or use the
globally most recent account. Several accounts may serve simultaneous requests.
Between requests, `last_successful_account` means **last used**, not currently
serving. No selection is reported before the first successful request, after a
server restart, or after 24 hours without a successful request. Failed retries
do not replace successful-account history.

## Connect an unmodified Codex CLI

Use the **same inference key** as the model provider. A management key is neither
required nor accepted as a substitute. With the inference key available in the
client environment:

```toml
[mcp_servers.proxy_usage]
url = "http://127.0.0.1:8317/v1/account-policy/mcp"
bearer_token_env_var = "CLI_PROXY_INFERENCE_KEY"
enabled_tools = ["proxy_account_usage"]
default_tools_approval_mode = "auto"
```

Alternatively, the provided Linux header helper reads a private JSON file owned
by the current user (mode 0600), containing `inference_key`. It emits only the
inference authorization header. The existing private Telegram companion config
has that field and can be reused locally; Telegram is not a runtime dependency.
Do not run the helper manually in a visible terminal or copy its output into a
tool result, log, repository, or chat.

```toml
[mcp_servers.proxy_usage]
url = "http://127.0.0.1:8317/v1/account-policy/mcp"
http_headers_helper = "node /DATA/AppData/CLIProxyAPI/client-usage/codex-auth-headers.mjs /DATA/AppData/CLIProxyAPI/telegram-companion/private/config.json"
enabled_tools = ["proxy_account_usage"]
default_tools_approval_mode = "auto"
```

Restart Codex / open a new session after adding the MCP entry. Use the server's
Tailscale address instead of loopback for another machine, and protect any
credential file there. Plain HTTP is appropriate only on loopback or a trusted
encrypted tailnet; use HTTPS on untrusted networks. A shared inference key is a
shared caller namespace, not independent user authorization.

## Backend contract

- `GET /v1/account-policy/usage?session_id=<thread UUID>`: the same report without
  MCP. Inference bearer authentication is required, even if anonymous inference
  is enabled elsewhere. No global-account fallback.
- `POST /v1/account-policy/mcp`: stateless Streamable HTTP with JSON responses;
  initialize, initialized notification, ping, tools/list, and tools/call only.
  No OAuth impersonation, account listing, reset tools, or mutable settings.
- `active_accounts`: exact session's live attempts, including overlapping
  requests; `last_successful_account`: most recently completed successful request.
- `weekly`: ordinary, model-independent 604800-second bucket only. Includes
  `used_percent`, `remaining_percent`, `window_starts_at`, `refresh_at` (UTC),
  and `observed_at`. `time_zone` is provided for presentation. Subscription
  renewal and saved manual reset expiry are **not** the weekly refresh time.
- Percentages and dates become null when evidence is absent, stale, invalid,
  ambiguous, or belongs to a previous identity. No fabricated zero values and
  no provider polling/reset writes during a tool read. Existing observation
  collection supplies fresh data; enable policy or observations as appropriate.
  Healthy discovery is scheduled at half the configured freshness window plus
  bounded per-account jitter (about 60-65 seconds at the default 120 seconds),
  including idle accounts. Provider-error backoff remains authoritative; reads
  can still honestly be unavailable during failures or in-flight discovery.
- Responses are `Cache-Control: no-store`. No provider credentials, workspace
  IDs, upstream errors, or management secrets are returned.

## Upstream integration and regression checks

All reporting/protocol logic lives in `internal/accountpolicyclient/`; the HTTP
adapter is `internal/api/server_account_policy_client.go`. Auth lifecycle tracking
lives in `sdk/cliproxy/auth/account_policy_client.go`. The upstream-facing hooks
are one route registration in `server.go`, one bounded-history field in Manager,
one completion callback in `MarkResult`, one sanitized selected-attempt metadata
hook, and the session key on the existing policy request lease. Provider discovery
and client reads share `PolicyAccountIdentity` without changing its validation.
No payload/translator/executor changes or passthrough
default changes are required.

When merging upstream, preserve those small hooks, especially successful-result
tracking and stream lease lifetime. Follow `docs/account-policy-upstream-updates.md`
for the fork-wide worktree/update/merge procedure; do not resolve whole upstream
files with `--ours`. Recheck:

```bash
go test ./internal/accountpolicyclient ./internal/api ./sdk/cliproxy/auth
go test -race ./internal/accountpolicyclient ./internal/api ./sdk/cliproxy/auth
go test ./...
go build -o test-output ./cmd/server
node --test client-usage/codex-auth-headers.test.mjs
```

Also smoke-test a real installed Codex version: successful MCP initialization
and tool discovery, followed by an exact-session call. Rollback is disabling
the MCP entry or using the previous proxy image; other inference behavior and
reset/routing settings remain unchanged.
