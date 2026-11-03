# Claude Code destination

Claude Code uses a dedicated base URL; every other client keeps the default destination.

| Destination | Base URL | Anthropic models | GPT/Codex models |
| --- | --- | --- | --- |
| Claude Code | `http://HOST:8317/claude-code` | listed and usable | listed and usable (existing translation) |
| Default | `http://HOST:8317` (`/v1`, `/backend-api/codex`, `/v1beta`) | never listed; rejected when restricted | unchanged |

Claude Code: `ANTHROPIC_BASE_URL=http://HOST:8317/claude-code`. It then calls
`/claude-code/v1/messages`, `/claude-code/v1/messages/count_tokens` and `/claude-code/v1/models`.

## How it works

- `internal/api/claude_code_destination.go` strips `/claude-code` from `/claude-code/v1/...`
  before Gin routing and marks the request context. The regular `/v1` routes, handlers and
  auth middleware serve the request. Only `/v1` is mounted under the prefix.
- `sdk/api/handlers/claude_code_destination.go` holds the filters:
  - Model lists (`WriteModelListResponse`: OpenAI, Codex `client_version`, Home, Grok, Gemini)
    drop Anthropic models on the default destination. Anthropic-format lists
    (`Anthropic-Version` / `claude-cli`) keep them until the restriction is enabled, so
    unmigrated Claude Code clients keep working.
  - Execution (non-stream, stream including WebSocket turns and bootstrap retries, count tokens,
    plugin-routed requests) rejects Anthropic models with HTTP 403
    `Anthropic models are only available via the Claude Code endpoint` when restricted.
- Anthropic models are identified by registry provider (`claude`/`anthropic`), registry
  ownership/type, the upstream name of compatibility aliases (`MetadataModelID`), then the
  `claude-` name prefix (for Home and third-party providers). Cloaked Anthropic-format IDs
  (`claude-fable-5-dd-...`) are decoded first, and both the suffix-stripped and full suffixed IDs
  are checked. `auto` is resolved first. Configured alias pools (API-key, OpenAI-compatibility
  and OAuth aliases mapping one alias to several upstreams) count as Anthropic if any upstream is
  (`Manager.ConfiguredAliasUpstreamModels`), because the registry keeps only one entry's metadata.
- Home catalogs are filtered by provider section before formatting. The flag-off exception
  applies only to responses served in Anthropic format (plain `/v1/models`, not Codex
  `client_version`, Grok or Gemini catalogs).

## Known limitations

- Home mode: the execution guard sees only the public model name. If CLIProxyAPIHome maps a
  neutral alias (no `claude-` prefix) to a Claude upstream, that request is not rejected.
  Home is disabled in production.
- A trailing-slash redirect (e.g. `/claude-code/v1/models/`) points at the unprefixed path,
  because Gin routes the stripped URL. Claude Code does not send trailing slashes.
- The config watcher follows the file path. Edit `config.yaml` in place (it is a single-file
  bind mount); editors that replace the file (`sed -i`) break the watch and the mount.

## Restriction flag

Default `false`. Hot-reloaded through the config watcher; no restart needed.

```yaml
# v8 layout (production)
upstream:
  claude:
    restrict-anthropic-models: true
# legacy layout
claude-code:
  restrict-anthropic-models: true
```

Enable it only after every client has moved Claude traffic to `/claude-code`.
