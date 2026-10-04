# Account policy implementation plan

Specification: `CLIProxyAPI-Agent-Spec.md`. Base: `8ef43e4df3b216a42493105d31c2873b69191473` (matches research snapshot). Initial worktree: `main`, modified `.dockerignore`, untracked specification. Preserve both.

## Audit and decisions

The backend checkout contains passive quota observation, affinity, websocket transports, and v8 management authentication. It contains no reset-credit discovery adapter or maintained frontend source. The management panel is a separately downloaded upstream artifact. Audit its maintained source before integrating; preserve that panel. A maintained, embedded policy page is permitted if that panel cannot be modified in this checkout.

Tailscale is running on `levi-thinkcentre-m700.dinosaur-dojo.ts.net`, IPv4 `100.82.251.30`. Verify locally over this interface with isolated fixtures. Do not alter host services, tailnet ACLs, credentials, or redeem live credits. Deliver service and ACL recipes and report which live deployment checks need operator provisioning.

Go is not on PATH. Use an isolated Go 1.26 toolchain under `/tmp/cliproxy-policy-toolchain` for verification.

### Updated automatic reset rule

The user's subsequent instruction supersedes any queued-work or near-expiry condition in the original specification. When fresh ordinary weekly allowance is exhausted, immediately redeem the earliest expiring eligible credit whose expiry is strictly before the next normal weekly refresh. No queued task or proximity to expiry is required. Credits expiring at or after that refresh remain saved under `auto_expiring`; explicit `auto_all_saved` retains its separately authorized saved-credit policy. The expiry safety deadline remains an independent fallback while allowance is still available. Successful redemption requires verified refreshed allowance and inventory, narrowly clears only the affected stale quota cooldown, reads the new weekly refresh, and reevaluates routing. Account identity, idle reservation, holds, read-only controls, durable idempotency, and unsupported-credit safeguards still apply.

## Frozen internal contracts

`internal/accountpolicy` must not import config, auth, or API packages. Export these JSON-safe types (JSON snake_case; settings YAML kebab-case):

- `Settings`: Enabled bool, Mode string (`earliest_deadline`), Automation string (`off`, `notify`, `auto_expiring`, `auto_all_saved`), Fallback string, Affinity string (`strict`, `deadline_at_boundary`), StateDir string, TimeZone string, ExpiryGuardSeconds int, FreshnessSeconds int, SavedCreditReserve int, ReadOnly bool, ForceAccount string, CreditTypes []string, Accounts map[string]AccountControl.
- `AccountControl`: Hold bool, ReservePercent float64.
- `Identity`: CredentialID, AccountID, WorkspaceID, Provider, Alias string; Generation uint64. CredentialID is the proxy credential mapping; AccountID is upstream ownership; WorkspaceID defaults to account identity only if the provider makes them equivalent.
- `Bucket`: Scope, Model string; DurationSeconds int64; UsedPercent float64; Allowed *bool; ResetAt, ObservedAt time.Time; Source string.
- `Credit`: ID, Type, Status, Title string; GrantedAt time.Time; ExpiresAt *time.Time; DetailsKnown bool; Scopes []string.
- `Snapshot`: Identity Identity; Version uint64; Plan, Status string; Eligible bool; Buckets []Bucket; Credits []Credit; AvailableCredits int; InventoryComplete bool; ObservedAt, InventoryObservedAt time.Time; LastError string; WritesDisabled bool; ActiveRequests, ActiveBindings int; Transport string.
- `Evaluation`: Eligible, Known bool; Deadline time.Time; Reason string.
- `Decision`: CredentialID, Provider, Model, Reason string; Deadline, At time.Time; Fallback bool.
- `Operation`: ID, RequestID, CredentialID, AccountID, WorkspaceID, CreditID, State, Result, Error string; CreatedAt, UpdatedAt time.Time; Before, After *Snapshot.
- `Schedule`: ID, CredentialID, CreditID string; At time.Time.
- `ConsumeResult`: Code string; WindowsReset int.
- `Provider` interface: `Discover(context.Context, Identity) (Snapshot,error)` and `Consume(context.Context,Identity,string,string) (ConsumeResult,error)`; final two arguments request ID and selected credit ID.
- `Options`: Settings Settings; Provider Provider; Accounts func() []Identity; Recover func(context.Context, Snapshot, Snapshot) error; Now func() time.Time; HasDemand func(string) bool; IsIdle func(string) bool; AcquireReset func(context.Context,string) (func(),error). AcquireReset is an atomic reset reservation sharing account concurrency state with the inference request lease; its release ends the reservation. A read-only idle check alone cannot prevent a new request starting during redemption.
- `NewService(Options) (*Service,error)`; `Service.Run(context.Context)` (blocking loop); `Service.Settings() Settings`; `Service.UpdateSettings(Settings) error`; `Service.Accounts() []Snapshot`; `Service.Decisions() []Decision`; `Service.Operations() []Operation`; `Service.Schedules() []Schedule`; `Service.Refresh(context.Context,string) error` (empty ID refreshes all); `Service.Redeem(context.Context,string,string) (Operation,error)`; `Service.Schedule(string,string,time.Time) (Schedule,error)`; `Service.CancelSchedule(string) error`; `Service.RecordDecision(Decision)`; `Service.Snapshot(string) (Snapshot,bool)`; `Service.Tick(context.Context) error` (deterministic scheduler); `Evaluate(Snapshot,string,Settings,time.Time) Evaluation`; `DefaultSettings() Settings`; `ValidateSettings(Settings) error`.
- `CodexClient`: `Credential func(context.Context,string)(CodexCredential,error)`; `RefreshCredential func(context.Context,string) error`; `HTTPClient *http.Client`; `HTTPClientForCredential func(string)*http.Client` for existing account-specific transports; `BaseURL string` (trusted deployment configuration only, default https://chatgpt.com); `PathStyle string` (chatgpt or codex). `CodexCredential`: AccessToken, AccountID, WorkspaceID string. Implements Provider. Fixture-only clients may inject a localhost base URL.

Core owns defaults, validation, persistent state, discovery, evaluation, redemption, scheduling. Adapter owns CodexClient and sanitized protocol fixtures. Routing owns auth selector, affinity, narrow cooldown recovery, and runtime signal reporting. Lead owns configuration and service lifecycle wiring. Operators own management handlers/registration options and proxyctl. UI owns embedded maintained page and asset handler, without editing server routing; operators register its route. Companion owns deployment/workflow material.

## Public API contracts

All paths use `/v8/management/account-policy`, existing management authentication, and stable `{ "error": { "code": "...", "message": "..." } }` failures.

- GET `/accounts`: `{settings, accounts}`.
- GET `/decisions`: `{decisions}`.
- GET `/resets`: `{operations, schedules}` (credit inventory is in account snapshots).
- PATCH `/settings`: partial settings JSON; returns `{settings}`. Never silently enable automatic writes.
- POST `/refresh`: `{credential_id}`; returns refreshed `{accounts}`.
- POST `/resets/redeem`: mandatory `{credential_id, credit_id}`; returns `{operation}`.
- POST `/resets/schedule`: mandatory `{credential_id, credit_id, at}` RFC3339 or RFC3339 with explicit offset; returns `{schedule}`.
- DELETE `/resets/schedule/:schedule_id`: `{cancelled:true}`.
- GET `/diagnostics`: sanitized `{settings,accounts,decisions,operations,schedules}`.
- UI `/account-policy.html`: maintained embedded page; it only obtains data and writes through authenticated v8 API calls.

## Packages and dependency checks

| Package | Shared interfaces | Verification |
| --- | --- | --- |
| A Adapter | Provider, identity, snapshots, consume result | Official pinned schema, identity mismatch, unknown expiry/types, selected ID/idempotency, no redirects leaking tokens |
| B Core | Settings, snapshots, journal, evaluation | Fake-clock deadlines, two-account scenario, stale evidence, reserve, discovery concurrency/backoff, restart/lost response, operation uniqueness |
| C Routing | Snapshot/Evaluate + auth candidate eligibility | Across priorities, fallback, disabled preservation, unsafe continuation, child affinity, narrow generation checked recovery |
| D Reset | Core + Provider + Recover callbacks | No live writes, journal-before-submit, one logical ID, reconciliation, cooldown persistence |
| E Operators/UI | Frozen v8 JSON contracts | Auth, input validation, sanitized diagnostics, CLI errors, Playwright interactions, countdown/DST/no tick polling |
| F Integration/companion | Config lifecycle and runtime hooks | Build, suite, race, transport fixtures, private tailnet smoke, services/workers/rollback documentation |

Package D is implemented together with B to keep journal and state-machine ownership atomic. Packages run in separate worktrees; integrate only reviewed commits. No overlapping concurrent edits. The lead performs final cross-package checks and fixes.

## Release and rollback

Upstream compatibility requirement: custom rules remain in separate `accountpolicy`,
`account_policy_*`, UI, usage and companion modules. Preserve upstream interfaces
and disabled defaults, use narrow optional hooks, avoid unrelated refactoring,
and document each integration point in `account-policy-upstream-updates.md`.

Ship disabled by default. Validate observation, routing, reset dry-run, then explicit automation. Never erase pending operation state on rollback. Preserve credentials and the owner's existing management panel. Live provider redemption and modification of tailnet ACLs are outside this implementation run.
