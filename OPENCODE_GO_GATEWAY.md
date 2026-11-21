# Remote OpenCode Go gateway connection

This deployment is prepared for one OpenAI-compatible gateway on another
Tailscale-connected PC. The gateway owns three OpenCode Go accounts and handles
random account selection, per-account limits, cooldowns and safe retries.
CLIProxyAPI exposes those models to its existing clients as `go/<model-id>`.

The live `config.yaml` contains a disabled provider named `opencode-go-remote`.
It has no credentials or models yet. Its `.invalid` URL is a deliberate inactive
placeholder. Plugins are not required. Existing account-policy routing remains
enabled for the existing accounts.

## Gateway contract

- Supply a reachable Tailscale base URL ending in `/v1`, for example
  `http://100.x.y.z:8080/v1` or an HTTPS URL using the PC's Tailscale DNS name.
  Give the base URL, not the full `/chat/completions` request URL.
- Support authenticated `POST /v1/chat/completions`, including streaming SSE,
  message history and tool calls. This proxy's OpenAI-compatible executor sends
  Chat Completions even when its client uses `/v1/responses`. A gateway that only
  exposes Responses needs a different adapter; do not enable this entry for it.
- Supply the exact model IDs. `GET /v1/models` is useful for discovery; this proxy
  uses an explicitly configured model list rather than discovering it automatically.
- Use a separate gateway bearer token. Keep the three actual Go account
  credentials on the remote PC. Permit this proxy host to reach the gateway in
  the Tailscale access policy and the remote PC's firewall.
- Preserve recognizable client User-Agent and session headers. The prepared
  provider forwards `X-Opencode-Session`, Codex `Session-Id` / `Session_id` and
  `Thread-Id`, and `X-Claude-Code-Session-Id` when supplied by the client. The
  gateway should derive a stable session from these when appropriate and preserve
  it on its upstream requests. Do not invent a new session ID for every retry.
- Return real HTTP status codes for rate limits and failures. Send `Retry-After`
  with a 429 where known. Account-specific cooldowns belong to the gateway;
  CLIProxyAPI sees the gateway as one upstream credential. Avoid returning a
  gateway-wide 429 while another account can safely serve the request.
- Retry or switch accounts before sending output to the client. After output
  starts, propagate stream failures. Account-bound reasoning or response IDs
  require their original owner; model names alone do not prove interchangeability.

## Activate when the gateway is ready

1. Back up private `config.yaml` outside the Docker build context. Edit it locally
   or use the authenticated v8 management configuration API. Keep file mode 0600.
2. Find `api-keys.openai-compatibility` and edit the existing
   `opencode-go-remote` entry. Set its real `base-url`, add the gateway token under
   `keys`, and add the actual `models`. Keep other provider entries intact.
   [opencode-go-provider.example.yaml](opencode-go-provider.example.yaml) shows
   the required shape. Its `REPLACE_WITH_...` values are not usable credentials or
   model names.
3. Keep `proxy-url: direct` on the gateway key so calls use the host's direct
   Tailscale route rather than a general outbound proxy. Keep cooling enabled.
   This entry allows one additional proxy retry round; the gateway owns the
   retries across its three accounts.
4. Set this entry's `disabled: false` only after filling in those values.
   Configuration is mounted into the container and watched for reload, so no
   source rebuild is needed. Verify the effective configuration and model list
   after the change. If the watch/reload fails, inspect the logs before restarting.

Client requests select `go/<model-id>`. The `go` prefix separates these models from
existing providers that may advertise the same model names. Inbound clients use
the existing CLIProxyAPI client key; the upstream gateway token is separate.

## Verify the real connection

Check from this deployment host and through the running container's request path:

1. Authenticated gateway model discovery, if implemented, and a fresh completion.
2. Authenticated proxy `/v1/models` lists the configured `go/` models.
3. Proxy `/v1/chat/completions` works for non-streaming and streaming requests.
4. Proxy `/v1/responses` works for a fresh request and a second turn containing
   the returned output. Verify tool-call/result forwarding with a fixture tool.
5. The remote gateway proves rotation across all three accounts and safe failover
   when one account is rate limited. The proxy preserves returned 429 status and
   cooldown behavior when all accounts are unavailable.
6. Access works through both the intended LAN and Tailscale client URLs. A healthy
   container or a mock gateway check alone does not prove the remote connection.

Keep keys, prompts and encrypted conversation state out of reports and Git.
Local preparation can be verified with an isolated fixture gateway, but live
OpenCode Go access remains unverified until the remote endpoint is supplied.

## Preparation verified on 2026-10-10

The production container loaded the disabled entry through config hot reload.
Its existing 14 models remained available, and loopback, LAN and Tailscale health
checks passed. No production restart or image replacement was needed.

An isolated container using the deployed image and the same Docker bridge
network passed gateway bearer authentication, four client session header formats,
non-streaming completions, fresh and second-turn Responses, both streaming
protocols with terminal events, tool-result forwarding, and 429 cooldown handling.
The fixture used synthetic credentials and temporary state and was removed after
the checks. Docker Compose configuration validation, provider example YAML
validation and the required server compilation also passed.

These checks prove local adapter readiness. They do not prove reachability of
the future remote PC, its three-account rotation, actual Go quota signals, or
live model access.

To deactivate, set only `opencode-go-remote.disabled: true` and verify reload.
Preserve the existing credentials, policy state and other provider settings.
