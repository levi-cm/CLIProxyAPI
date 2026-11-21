# AGENTS.md

Go 1.26+ proxy server providing OpenAI/Gemini/Claude/Codex compatible APIs with OAuth and round-robin load balancing.

## Repository
- GitHub: https://github.com/router-for-me/CLIProxyAPI

## Deployment
- The production router is built and deployed from the `feat/usage-split-scheduler` worktree (`.worktrees/usage-split-20261111`). After verifying changes and receiving the user's explicit go-ahead, prepare and launch the automatic idle deployment using `.worktrees/usage-split-20261111/ops/idle-deploy/README.md`. Keep the idle, active-request, state-preservation, and rollback safeguards enabled.
- Standing exception (operator, 2026-11-15, extended 2026-11-18): the daily upstream watch may act without a fresh go-ahead, strictly under `.worktrees/usage-split-20261111/ops/upstream-watch/README.md`. Protected routing paths hard-block every deploy, and full gates plus Codex approval are required. Launches go only through this runner.
  - Class A/B upstream fixes: idle `main.py launch --now`.
  - New Codex/Claude model support: catalog refresh, Codex client upgrade, and class M fixes.
  - Advisor switch to the newer model of the same family: `prepare --config` limited to `subagent-advisor.models`.
  - Class M and advisor-switch releases may use `interrupt.py`, with outage budget ≤ 70, one per run, launched 04:00–07:00. Before launch, fresh reviewers from different model families (GPT and Claude, plus DeepSeek when available) must all approve.
- Every deploy is automatic through that runner. Never run a manual `docker compose up`/`restart` (or `stop`/`rm`) against production for a cutover. Config changes that ship with a deploy are staged with `prepare --config`.
- The runner must start CLIProxy after the deploy and verify it within about 60s: State `running`, `/healthz` 200 and an authenticated `/v1/models` 200. On any failure it rolls back automatically (previous `.env` pin, `config.yaml` and image), verifies that the same way and keeps retrying until it serves. Read the outcome from its `status.json`; never print success without a passing check.
- All agents route their model traffic through this proxy, so the launching agent will lose its connection during the cutover. The runner is a detached systemd unit with an independent watchdog and must never depend on the launching agent staying alive.
- Dry runs must exercise the same compose path: the production compose files under a separate project and spare ports (`main.py compose-dryrun`, also part of `prepare`), proving the container reaches State `running` and serves.

## Project file organization

- Keep everything owned by this CLIProxyAPI project inside `/DATA/AppData/CLIProxyAPI` or its subdirectories. Do not create project files in sibling folders, elsewhere under `/home/levi`, or in `/tmp`.
- This includes isolated checkouts and Git worktrees, project-specific tools and toolchains, configuration, credentials, durable state, logs, caches, test fixtures, temporary files, build outputs, downloads, reports, screenshots, archives, and backups. Shared system tools and their system-managed files remain in their normal locations.
- Use appropriate subfolders instead of cluttering the project root: `.worktrees/` for isolated worktrees, `.tmp/` for scratch files and temporary fixture state, `output/` for generated artifacts, `docs/` for documentation and reports, and `scripts/` for reusable helpers. Keep generated files, credentials, state, and nested worktrees excluded from Git and Docker build contexts as appropriate.
- Keep project-specific toolchains in `.tools/`. Set `TMPDIR=/DATA/AppData/CLIProxyAPI/.tmp` for project build and test processes so their generated scratch files stay inside the project as well.
- This location rule does not authorize moving active production files or changing live paths. Before relocating existing files, check references, mounts, services, and ownership; preserve zero downtime, streams, WebSockets, credentials, durable conversation bindings, and automatic refills. Keep development and fixture state separate from production state, and never start a second production-state writer.

## Commands
```bash
gofmt -w . # Format (required after Go changes)
./scripts/go-limited.sh build -o cli-proxy-api ./cmd/server # Build
./cli-proxy-api # Run the built dev server
./scripts/go-limited.sh test ./... # Run all tests
./scripts/go-limited.sh test -v -run TestName ./path/to/pkg # Run single test
./scripts/go-limited.sh build -o test-output ./cmd/server && rm test-output # Verify compile (REQUIRED after changes)
```
- Compilation must use `scripts/go-limited.sh`: half the available CPUs (rounded down, minimum one), `GOMAXPROCS` set to that budget, and one Go package compiled at a time. Linux compiler/linker/CGO processes also share that CPU affinity limit. This does not change production runtime CPU settings. Set `GO_BINARY` only when Go is not on `PATH`.
- Common flags: `--config <path>`, `--tui`, `--standalone`, `--local-model`, `--no-browser`, `--oauth-callback-port <port>`

## Config

- Production automatic reset redemption must remain enabled. Never select `off` or `notify`, disable the policy, clear `automation_required`, or make the authoritative writer read-only to suspend redemption. Preserve the mandatory expiry guard and supported reset credit type. Keep replica ownership and account identity checks intact. Do not disable automation for builds, tests, updates, or rollback; use isolated fixture state.

- Default config: `config.yaml` (template: `config.example.yaml`)
- `.env` is auto-loaded from the working directory
- Auth material defaults under `auths/`
- Storage backends: file-based default; optional Postgres/git/object store (`PGSTORE_*`, `GITSTORE_*`, `OBJECTSTORE_*`)

## Architecture
- `cmd/server/` — Server entrypoint
- `internal/api/` — Gin HTTP API (routes, middleware, modules)
- `internal/api/modules/amp/` — Amp integration (Amp-style routes + reverse proxy)
- `internal/thinking/` — Main thinking/reasoning pipeline. `ApplyThinking()` (apply.go) parses suffixes (`suffix.go`, suffix overrides body), normalizes config to canonical `ThinkingConfig` (`types.go`), normalizes and validates centrally (`validate.go`/`convert.go`), then applies provider-specific output via `ProviderApplier`. Do not break this "canonical representation → per-provider translation" architecture.
- `internal/runtime/executor/` — Per-provider runtime executors (incl. Codex WebSocket)
- `internal/translator/` — Provider protocol translators (and shared `common`)
- `internal/registry/` — Model registry + remote updater (`StartModelsUpdater`); `--local-model` disables remote updates
- `internal/store/` — Storage implementations and secret resolution
- `internal/managementasset/` — Config snapshots and management assets
- `internal/cache/` — Request signature caching
- `internal/watcher/` — Config hot-reload and watchers
- `internal/wsrelay/` — WebSocket relay sessions
- `internal/usage/` — Usage and token accounting
- `internal/home/` — CLIProxyAPIHome control plane integration (bootstrap, RESP communication, dispatch coordination)
- `internal/tui/` — Bubbletea terminal UI (`--tui`, `--standalone`)
- `sdk/cliproxy/` — Embeddable SDK entry (service/builder/watchers/pipeline)
- `test/` — Cross-module integration tests

## Code Conventions
- Keep changes small and simple (KISS)
- Comments in English only
- If editing code that already contains non-English comments, translate them to English (don’t add new non-English comments)
- For user-visible strings, keep the existing language used in that file/area
- New Markdown docs should be in English unless the file is explicitly language-specific (e.g. `README_CN.md`)
- As a rule, do not make standalone changes to `internal/translator/`. You may modify it only as part of broader changes elsewhere.
- If a task requires changing only `internal/translator/`, run `gh repo view --json viewerPermission -q .viewerPermission` to confirm you have `WRITE`, `MAINTAIN`, or `ADMIN`. If you do, you may proceed; otherwise, file a GitHub issue including the goal, rationale, and the intended implementation code, then stop further work.
- `internal/runtime/executor/` should contain executors and their unit tests only. Place any helper/supporting files under `internal/runtime/executor/helps/`.
- Payload configuration MUST be the final semantic barrier before sending requests in every executor, including streaming, WebSocket, continuation, retry/fallback, image, and token-count paths where applicable. Complete all built-in payload translation, normalization, injection, and cleanup first, then evaluate and apply user payload rules exactly once to the final business payload. No subsequent logic may overwrite configured values or restore filtered fields. Only necessary transport framing, serialization, signature calculation, and read-only validation may follow without changing business payload semantics. Add regression tests when introducing or changing request-building paths to enforce this ordering.
- Follow `gofmt`; keep imports goimports-style; wrap errors with context where helpful
- Do not use `log.Fatal`/`log.Fatalf` (terminates the process); prefer returning errors and logging via logrus
- Shadowed variables: use method suffix (`errStart := server.Start()`)
- Wrap defer errors: `defer func() { if err := f.Close(); err != nil { log.Errorf(...) } }()`
- Use logrus structured logging; avoid leaking secrets/tokens in logs
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes
- Timeouts are allowed only during credential acquisition; after an upstream connection is established, do not set timeouts for any subsequent network behavior. Intentional exceptions that must remain allowed are the Codex websocket liveness deadlines in `internal/runtime/executor/codex_websockets_executor.go`, the wsrelay session deadlines in `internal/wsrelay/session.go`, the management APICall timeout in `internal/api/handlers/management/api_tools.go`, and the `cmd/fetch_antigravity_models` utility timeouts
- Avoid wall-clock `time.Sleep` in TTL, expiration, ordering, or cache-eviction unit tests due to platform timer granularity (e.g. Windows default timer resolution of ~15.6ms) and CI jitter under load; prefer controllable clocks (`nowFunc` / mock clock), explicit timestamp manipulation, or deterministic synchronization primitives.
- Note: if modifying features that involve CLIProxyAPIHome, check if corresponding updates are needed in the CLIProxyAPIHome repository.
- Endpoints under the `/v0/management` base URL are deprecated and no longer maintained. For any feature changes, do not modify endpoints under `/v0/management` unless necessary to fix compilation errors.
