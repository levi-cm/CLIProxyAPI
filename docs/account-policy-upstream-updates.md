# Keeping the account-policy fork mergeable

The implementation base is `8ef43e4df3b216a42493105d31c2873b69191473`.
The feature is optional and ships disabled with automatic redemption off.
Upstream `Selector`, executor, credential, HTTP, and usage plugin interfaces are
preserved; extension capabilities are additive optional interfaces or callbacks.
No executor payload-building or translator code was changed. The downloaded
management panel remains upstream-owned and is not patched after download.

The independent Telegram companion is a locally excluded nested repository at
`telegram-companion/`, not proxy routing code or an upstream submodule. Its only
additional contract is two v8 account-policy endpoints: authenticated capability
version1 and snapshot-bound caller-idempotent confirmed redemption. Preserve
`internal/accountpolicy/confirmed_reset.go`, the optional confirmation hooks in
`reset.go`, `management/account_policy_companion.go` and two registrations in
`server_account_policy.go`. Original redemption, scheduler, API/Go interfaces and
defaults are unchanged. Run `TestConfirmed`, `TestCompanion` and server route
regressions after merging; run the companion adapter checks independently.
Companion failure/rollback does not stop the proxy. No deprecated v0 changes.

## Module boundaries

| Custom module | Responsibility |
| --- | --- |
| `internal/accountpolicy/` | Provider protocol, observations, pure evaluation, durable reset journal and scheduler; no config/auth/API imports |
| `sdk/cliproxy/auth/account_policy_*.go` | Native routing adapter, affinity evidence, request/reset leases and narrow cooldown recovery |
| `sdk/cliproxy/service_account_policy.go` | Optional lifecycle/config/credential wiring and ownership checks |
| `internal/api/server_account_policy.go` | Extension routes, assets and server options |
| `internal/api/handlers/management/account_policy*.go` | Authenticated v8 controls, sanitized diagnostics and read-only dashboard telemetry |
| `internal/accountpolicyui/` | Maintained embedded extension, without changing the upstream panel |
| `internal/accountpolicyusage/` | Private bounded allowlisted usage records and cached local dashboard aggregates through the existing usage plugin interface |
| `internal/accountpolicybindings/` | Private atomic conversation ownership state; caller/session/model isolation and ownership CAS |
| `cmd/proxyctl/`, `cmd/account-policy-preview/`, `companion/` | Operator CLI, fixture-only browser preview and separate fleet workflow |

Keep business rules in these modules, not in upstream hooks. Avoid renaming,
moving, or reformatting unrelated upstream functions when updating the fork.

Preserve the durable binding hooks in `account_policy_selector.go` and the fresh
credential resolver in `service_account_policy.go`. See
`docs/account-policy-conversation-ownership.md` for resume/fork validation and
recovery. The independent client usage endpoint/MCP adapter and completion tracking
hooks are documented in `client-usage/README.md`; they do not infer conversation owners.

## Integration points to recheck after a merge

| Upstream surface | Small integration to preserve | Regression gate |
| --- | --- | --- |
| `internal/config/{config,config_load,config_v8,parse}.go` | Optional settings, defaults/validation, deep cloning, zero-value YAML omission | `go test ./internal/config ./internal/watcher` |
| `sdk/cliproxy/{builder,service,service_config,service_lifecycle,service_auth,service_plugins}.go` | Construct/start/stop extension; lazy selector installation; quota/credential wake-up; Home/plugin conflict guard; usage drain | `go test ./sdk/cliproxy ./sdk/cliproxy/usage` |
| `sdk/cliproxy/auth/conductor_selection.go` | Optional all-priority candidates and retained selector ownership | Auth selector/affinity tests |
| `sdk/cliproxy/auth/selector.go` | Optional bound/parent/unavailable correctness hooks, scoped affinity namespace; original fallback for ordinary selectors | Auth continuation/child-session tests |
| `sdk/cliproxy/auth/conductor*.go` | Account request leases, stream-lifetime release, optional relevant-quota refresh signal | Auth runtime/reset/recovery tests |
| `sdk/cliproxy/usage/manager.go` | Additive completion-drain waiting; existing plugin signature unchanged | `go test ./sdk/cliproxy/usage` |
| `internal/api/{server,server_options,server_management_v8}.go` and handler fields | Attach extension and call separate route registrars; upstream management middleware unchanged | `go test ./internal/api ./internal/api/handlers/management` |
| `sdk/cliproxy/builder.go` and management handler fields | Optional account runtime source for `/v8/management/account-policy/dashboard`; activity available even when policy is off | Dashboard auth/no-write/runtime tests |
| `sdk/config/` and `config.example.yaml` | Additive public aliases and commented optional configuration | Config round-trip tests |

Some edits to struct fields cause necessary `gofmt` alignment changes. Treat those
as formatting, not a reason to replace upstream declarations wholesale. Preserve
new upstream fields during conflict resolution. Never resolve conflicts with a
blanket `--ours`/`--theirs` over these files.

## Update procedure

Dashboard collection is a separate optional gate: `ObservationsEnabled` maps to
`account-policy.observations-enabled` in YAML and `observations_enabled` in v8
settings. It permits discovery and the private SDK usage sink, not selector
installation, redemption, schedule execution or cooldown recovery. Keep these
authority checks separate when merging upstream changes. Both gates default off;
the observations flag is taken from config on restart (including false), avoiding
legacy journals silently losing a new explicit opt-in. No Redis queue aggregation
or executor telemetry hook is required.

1. Choose an upstream release/tag or reviewed commit explicitly. Back up the
   deployed binary and private state directory without committing credentials.
   Pause automatic writes for deployment; preserve pending operation UUIDs.
2. Keep the existing working checkout and its uncommitted edits untouched. Create
   a new update worktree from the reviewed fork tip. Replace `UPSTREAM_VERSION`
   and `UPSTREAM_TAG` below with the selected release identifiers.

```bash
# Run remote add only if an upstream remote does not already exist.
git remote add upstream https://github.com/router-for-me/CLIProxyAPI.git
git fetch upstream --tags
git worktree add -b update/cliproxy-UPSTREAM_VERSION ../CLIProxyAPI-update main
cd ../CLIProxyAPI-update
git merge --no-ff --no-commit UPSTREAM_TAG
```

3. Resolve each conflict using the integration table: retain upstream behavior
   and keep only the optional hook. Update custom adapters if a supported upstream
   interface changed. Do not copy old executor request-building paths over newer
   upstream code. Recheck payload rules still run as the final semantic barrier.
4. Use the Go version required by the new upstream `go.mod` (current base: Go
   1.26). Run these gates before committing the merge:

```bash
gofmt -w .
git diff --check
go test ./...
go test -race ./internal/accountpolicy ./internal/accountpolicyusage ./internal/accountpolicyui ./internal/api ./internal/api/handlers/management ./sdk/cliproxy ./sdk/cliproxy/auth ./sdk/cliproxy/usage ./cmd/proxyctl ./cmd/account-policy-preview
node --test internal/accountpolicyui/*test.cjs
python3 -m unittest discover -s companion -v
go build -o test-output ./cmd/server
rm test-output
```

5. Run the fixture preview and repeat Playwright login, ownership-confirmation,
   scheduling/cancellation, selected redemption, disabled/read-only states,
   countdown/no-provider-polling, DST and mobile checks. Verify simultaneous
   serving accounts, stale/unavailable recovery, unsaved forms, measured graph
   coverage and a 100-account paginated pool. Re-run the real transport fixtures
   on loopback and the intended private interface:

```bash
go test ./internal/api -run '^TestAccountPolicy' -count=1
# Replace this IP only after checking that it belongs to the deployment host.
ACCOUNT_POLICY_TEST_LISTEN=100.82.251.30:0 go test ./internal/api -run '^TestAccountPolicy' -count=1
go run ./cmd/account-policy-preview -listen 127.0.0.1:18318
```

6. Review and commit the merge in the update worktree. Integrate it into the fork
   via the normal reviewed PR/merge process; this procedure does not push or
   overwrite a dirty checkout. Stage deployment with observation and automation
   off, then enable routing and explicitly reviewed automatic redemption. Record
   the new upstream base and any changed hook in this document.

Do not run a live redemption as an update gate. Fixture tests cover provider
outcomes and idempotency without spending credits. Production tailnet/device ACL
checks and service provisioning are separate operator-controlled deployment gates.
Preserve durable consume-contract write guards during updates; ordinary healthy
usage reads do not authorize clearing them. Legacy schedules lacking stored
upstream ownership fail closed and must be reviewed/recreated explicitly.

## Rollback

Set `account-policy.enabled: false` and `automation: off` in YAML, reload/restart,
and restore the previous reviewed binary if necessary. YAML disabled/off remains
the restart authorization ceiling over saved operator settings. Keep account
credentials, the journal, schedules and pending UUIDs. Do not delete state to
make an update pass: uncertain writes must reconcile before any new redemption.
Validate the original routing behavior with the disabled regression tests.
