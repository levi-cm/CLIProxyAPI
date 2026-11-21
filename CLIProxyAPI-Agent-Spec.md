# CLIProxyAPI account routing and reset automation specification

## 1. Assignment

This document is the complete handoff to the owner's local implementation agent. The authoring session produced this Markdown file only. The local agent must implement and verify the changes specified below in the owner's environment.

Local agent instructions:
* Read this entire specification and the checkout's current AGENTS.md.
* Use the owner's existing repository when available. Record its version and working tree state. Preserve existing changes and credentials.
* Complete the required proxy, reset, recovery, API, CLI, and UI changes. Reuse existing capabilities before adding code.
* Deliver the separate companion deployment and workflow material described below. Reuse existing agent tools where available; do not rebuild their unrelated features inside the proxy.
* Work through the packages and acceptance cases. Do not stop after analysis, scaffolding, or a proposed plan.
* Use reasonable implementation choices where the specification leaves internal details open. State material backend limitations instead of simulating unsupported behavior.
* Return a runnable local result, test evidence, configuration instructions, and rollback steps. Publishing, upstream issue creation, paid resets, and live credit redemption are not required to complete implementation.

Extend https://github.com/router-for-me/CLIProxyAPI for a private Codex account pool. Use the existing proxy, credential manager, transport code, and management API.

Mandatory deployment requirement: this proxy will be used over Tailscale. The owner's computers and remote agent workers must reach the central proxy through the owner's private tailnet. Design the inference endpoint, management UI, and operator controls for this deployment from the start.

The owner has two Codex accounts. Account A has four days until its normal weekly reset. Account B has seven days until its normal weekly reset, but B has an unused reset credit that expires tomorrow.

Without an expiring credit, prefer the account whose usable weekly allowance will reset first. With B's expiring credit, prefer B's current allowance before the credit expires. Redeem that credit after useful depletion or before its expiry safety deadline. Then read the new limits and calculate the account order again.

Optimize useful completed work and prevent avoidable loss of allowance or reset credits. Preserve request correctness, account isolation, and reliable recovery.

Deliver a small Go fork with an optional native policy module. Keep the existing default behavior when the module is disabled. Do not replace the proxy with another gateway. The transcript's agent management features belong in a companion workflow.

## 2. Research baseline

Research date: 4 October 2026.

CLIProxyAPI main commit inspected:
8ef43e4df3b216a42493105d31c2873b69191473

Latest release returned during research:
v8.0.13, published 3 October 2026.

Official Codex main commit inspected:
afb436df8b70bb5bc57b86d9a3e829968988cd21

These are research snapshots. At implementation start, record the actual fork base and compare it with these snapshots. Do not assume every feature on main is in the release binary. Read AGENTS.md and the relevant tests before changes.

Current implementation update from the owner, 4 October 2026:

The current UI already shows manual reset expiry per provider. It displays a time zone label, an absolute expiry, and a relative countdown for each reset. Treat this display as an existing capability. Preserve it and implement only the missing integrations or corrections identified during the local audit.

Reported UI values at the time of the update:

| Display entry | Displayed expiry under GMT+2 | Displayed countdown |
| --- | --- | --- |
| Reset 1 | 10/05, 06:18 | in 12 hours |
| Reset 2 | 10/22, 22:27 | in 18 days |
| Reset 3 | 10/29, 19:48 | in 25 days |

These are observed display values, not fixed future schedules or backend credit IDs. The local implementation has not been inspected in this authoring session. The display confirms visibility; it does not by itself prove that deadline routing, selected credit redemption, or cooldown recovery is implemented.

Audit the existing UI component, API response, provider adapter, timestamp representation, credit IDs, account ownership, and refresh path. Reuse that data flow. Do not scrape or parse rendered countdown text for scheduling. If the UI groups resets by provider, keep that grouping, but ensure each reset is linked to its owning account and workspace before routing or redemption. Do not interpret three reset rows as three accounts.

Any requirement below that is already satisfied becomes a preservation and verification requirement. Do not create a second reset list, duplicate adapter, or replacement dashboard merely to satisfy this specification.


Confirmed repository behavior:

| Area | Existing behavior | Required work |
| --- | --- | --- |
| Account selection | Round robin, weighted round robin, fill first, static priority | Add dynamic deadline policy |
| Session affinity | Available, disabled in the example configuration; established bindings outrank recovered higher priority accounts | Enable and verify; define controlled migration |
| Codex WebSocket | Existing executors; automatic selection requires downstream WebSocket and an enabled credential | Configure, test, and report actual transport |
| Quota observations | Passive HTTP and WebSocket signals exist; local UI already displays manual reset expiry | Audit and reuse local discovery; add only missing structured account and credit fields |
| Local cooldown reset | Existing management action and plugin callback | Reuse after verified provider recovery |
| Management UI | Upstream panel is separate; local implementation already shows manual reset entries and expiry countdowns | Preserve the local display and add missing policy controls and status |
| Usage statistics | README says built in statistics were removed in v6.10.0 | Use a supported usage sink for durable statistics |

Do not treat fill first as reset deadline routing. Do not treat a local cooldown reset as a provider reset. Do not describe affinity or WebSockets as missing features.

Issue 5782 was closed as not planned. The maintainer said quota dependent routing is outside the intended core behavior. This specification is for the owner's optional fork. Keep changes isolated.

## 3. Transcript coverage and scope

| Transcript idea | Deliverable |
| --- | --- |
| Prefer allowance that resets soonest | Normal routing policy |
| Account and session affinity | Stable bindings and controlled migration |
| WebSocket performance | Configuration, transport verification, regression coverage |
| Better dashboard and easier setup | Account policy panel and setup checks |
| One persistent proxy for several machines | Service deployment and private remote access |
| Tailscale access | Mandatory private tailnet deployment for inference, UI, management, and remote workers |
| Cache visibility | Supported usage sink and honest cache metrics |
| Normal, immediate, and banked resets | Separate event types and provider specific semantics |
| Work while the laptop is closed | Remote worker deployment |
| Fleet setup repository | Companion setup documentation and repeatable configuration |
| Parallel agent tasks | Isolated worktrees and work package contracts |
| Pending messages and immediate steering | Existing agent tool integration or a separate companion queue |
| Task inbox, completed work, archive, snooze, search | Companion workflow; do not put conversation storage in the proxy |
| Remote worker selection and load distribution | Companion orchestration with worker capacity limits |
| PR verification, review, previews, and recovery | Implementation handoff and rollback requirements |
| Prevent several builds from exhausting one host | Companion CI concurrency limits |

The sponsor segment, claimed subscription value, fixed cache lifetime, model rankings, and claims about avoiding bans are not software requirements. Do not use transcript estimates as provider contracts.

Codex is the first provider. Design adapters for later providers. Implement Claude reset behavior only after current source or documentation verifies it. Keep model selection explicit. Do not substitute a cheaper model without the owner's configuration.

## 4. Definitions and stored state

Store timestamps in UTC. Display them in Europe/Berlin by default. Accept an explicit offset or named zone for manual inputs.

Keep these events separate:

1. Normal quota reset: a provider window refreshes at its scheduled time.
2. Banked reset credit: a redeemable benefit that may have an expiry.
3. Automatic provider reset: the provider applies it without redemption.
4. Local cooldown clear: the proxy removes a routing block.
5. OAuth refresh: credentials are renewed; quota is not increased.
6. Purchased reset: a separate financial action. It is outside automatic redemption scope.

Maintain a versioned account snapshot with:
* Logical account and workspace identity, provider, credential mapping, and display alias.
* Authentication health, eligibility, model restrictions, cooldown reason, and generation.
* Each quota bucket's scope, duration, used percentage, allowance status, reset time, observation time, and source.
* Credit inventory, observation time, whether details are complete, and each credit's opaque ID, type, status, grant time, and expiry.
* Policy mode, account reserve, manual controls, active sessions, actual transport, and latest decision.
* Pending redemption record, stable request ID, state, and recovery evidence.

Identify windows from their duration and scope. Do not assume primary always means five hours or secondary always means one week.

Keep ordinary allowance, model specific allowance, code review allowance, purchased credits, and reset credits distinct. Token counts and request counts are not exact subscription allowance measurements.

Distinguish missing credit details from an explicitly nonexpiring credit. Official Codex represents missing details separately from a detailed credit whose expiry is null. A count alone does not provide expiry times. The detail list may be shorter than the count.

Use the existing credential manager for token refresh and persistence. Do not independently rewrite OAuth files.

## 5. Codex discovery and redemption adapter

The official Codex source contains the required read and write operations. Use it as the protocol reference. Audit the existing local reset adapter first. Reuse it when it meets the account identity, detailed inventory, selected credit, and idempotency requirements. Extend missing behavior instead of adding a duplicate provider integration.

For the ChatGPT backend path style:
* Read usage through GET /backend-api/wham/usage.
* Read credit details through GET /backend-api/wham/rate-limit-reset-credits.
* Redeem through POST /backend-api/wham/rate-limit-reset-credits/consume.

The official client also has an /api/codex path style. Use the path style associated with the authenticated backend. Do not assemble arbitrary URLs from untrusted account metadata.

A consume request carries redeem_request_id and can carry credit_id. Generate one UUID for one logical redemption. Persist it before sending. Reuse it for retries and recovery.

Official response codes include reset, nothing_to_reset, no_credit, and already_redeemed. An HTTP success response alone does not prove quota changed.

An alternative official CLI integration is the app server method account/rateLimitResetCredit/consume. Its parameters are idempotencyKey and optional creditId. Use isolated account state if this adapter is selected. Do not invent a codex reset command or control interactive menus with terminal keystrokes.

Preferred implementation: a Go backend adapter through the existing account aware HTTP and credential infrastructure. Keep the official app server adapter as an optional alternative if it simplifies account handling. Browser automation is a last resort if the account's backend cannot support the required operations.

Validate account and workspace identity on reads and writes. Never redeem against whichever account is open in a shared browser or CLI profile.

These routes are observed official client interfaces, not a promise of a permanent public API. Pin sanitized fixtures and detect schema changes.

## 6. Discovery service

Run discovery independently of inference selection. Request handlers read snapshots; they do not poll the provider or redeem credits.

Default targets:
* Usage refresh every 60 seconds for active accounts.
* Usage and inventory refresh every five minutes for idle accounts.
* Credit detail refresh every 60 seconds within one hour of a known expiry.
* Immediate refresh at startup, after credential changes, after redemption, and when a relevant provider event arrives.
* Evidence is fresh for automatic decisions for at most 120 seconds.

Use at most one discovery operation per account at a time and at most two across the initial pool. Add jitter and backoff. Honor rate limit responses. Keep failed reads visible as stale or unknown.

Only one service may own automatic redemption for a logical account. The initial deployment uses one proxy instance. Other instances must be read only for reset automation unless a shared lease and durable operation journal are implemented.

Discovery must include accounts that are in local quota cooldown. Otherwise the service cannot detect that a manual or automatic reset restored them.

Reuse passive quota observations as evidence. Do not infer reset inventory from inference headers that contain only quota or purchased credit information.

Use the repository's existing management request cancellation rules. Do not introduce new timeouts into established inference streams.

## 7. Routing policy

Create the optional policy mode earliest_deadline. Leave existing strategies unchanged.

For each requested provider and model:

1. Apply access controls, selected account pool, model compatibility, authentication health, known quota blocks, reserves, and concurrency limits.
2. Calculate each eligible account's relevant natural weekly deadline.
3. Calculate the safe redemption deadline for its earliest eligible expiring credit.
4. Use the earlier relevant deadline as the account's allowance priority.
5. Prefer the smallest deadline. Use configured priority, then stable account identity to break equal deadlines.
6. Respect session affinity under the rules in section 8.
7. Record the selection reason and evidence freshness.

A reset credit is relevant only if its type covers the current allowance, the account supports redemption, and the owner enabled automatic use of that credit class. Ignore unknown reset types.

For known credit expiry E, calculate safe redemption time D as E minus the expiry guard. The default guard is ten minutes. It is a safety margin, not a promise of exact optimal timing.

Missing or stale evidence must not invent deadlines. Delegate to the configured existing fallback strategy and report the reason. Known ineligible accounts remain excluded.

A depleted short window makes an account unavailable for work even when its weekly allowance remains. Prefer another eligible account. Do not redeem an ordinary saved credit just to bypass a short window unless the owner selected that policy.

Manual controls must support account hold, forced account selection, reserve percentage, scheduled redemption, and a selected credit. Force selection does not override real ineligibility.

A manual schedule is a local policy input. It cannot change provider expiry. Validate it against the selected credit and show the provider expiry separately.

## 8. Affinity, cache, and safe migration

Keep stable affinity during ordinary use. New sessions follow the deadline policy. Do not distribute every tool call across accounts.

Scope bindings by client identity, provider, logical session, model constraints, and credential generation. Reuse existing session extraction. Preserve prompt cache keys and lineage when valid. Keep child sessions on the parent account by default.

Expose two modes:
* strict: retain an eligible binding; deadline preference affects new assignments.
* deadline_at_boundary: permit migration between completed requests when an earlier allowance deadline requires it and the next request is replayable.

Use deadline_at_boundary for the owner's deadline policy. Migration requires an explicit correctness check. A request that depends on previous_response_id, account specific encrypted reasoning, turn state, or a live WebSocket is not automatically replayable.

Never move a response while it streams. Never splice two accounts into one response. Never replay completed tool effects to reconstruct context.

For a safe migration, release or replace the binding, create the destination transport session, and replay only valid conversation input. Record the account change and its reason. Keep unknown or unsafe continuation on its existing account and report why it could not move.

If no existing thread can safely move to B, new work must use B. The companion system can start approved independent jobs there. Report residual allowance that affinity prevented the system from using. Do not promise that all traffic can move without a compatible client.

Measure cache reads, cache writes, switches, and first token latency only where the provider and usage sink expose them. Mark unsupported metrics as unavailable. Do not assume every cache lasts five minutes.

## 9. Automatic reset scheduler

Automatic redemption is disabled by default. Setup offers auto_expiring as the recommended mode for this owner.

Modes:
* off: discover and display only.
* notify: calculate routing and redemption advice.
* auto_expiring: redeem eligible saved credits with known expiry.
* auto_all_saved: also use nonexpiring saved credits under configured reserve and demand rules.

Purchased resets are never redeemed or bought automatically.

For each authorized expiring credit:
1. Prefer useful work on its account before D.
2. Redeem early if its applicable weekly allowance is exhausted and useful work is waiting.
3. Otherwise wait until D to preserve more current allowance.
4. At D, redeem if fresh evidence shows at least one covered window can be refreshed, even if the usage threshold was not reached.
5. Select the earliest expiring eligible credit by its credit ID.
6. Recheck inventory and account identity immediately before the write.
7. Acquire the account redemption lock and persist the operation.
8. Consume, reconcile, verify quota recovery, and update routing.

Do not require full exhaustion before D. That would lose credits when the available workload cannot consume the account in time.

If there is nothing to reset, retain the credit and report that result. Do not create artificial requests solely to make redemption possible.

Nonexpiring credits remain saved in auto_expiring mode. In auto_all_saved mode, preserve one credit by default and redeem only for applicable exhaustion with approved work waiting. Known expiring credits may bypass this reserve.

Do not spend another credit while one redemption is unresolved. Do not loop through several credits after the first reset. Multiple credits require separate evidence that subsequent useful work depleted allowance again.

The scheduler is a documented heuristic. It cannot know exact future demand or guarantee a global optimum.

## 10. Redemption state and recovery

Use durable operation states:
planned, prepared, submitted, verifying, confirmed, nothing_to_reset, no_credit, outcome_unknown, failed, expired.

Persist before submission. Recovery after restart loads the same operation and request ID.

Handle results as follows:
* reset: verify the inventory and affected quota windows.
* already_redeemed: reconcile the same operation; do not submit a new ID.
* nothing_to_reset: keep the credit; reconsider only after fresh evidence changes.
* no_credit: refresh inventory and invalidate the unavailable selection.
* network failure after submission: outcome_unknown; reconcile or retry the same logical request.
* rejected authentication: use normal credential refresh once when appropriate; persistent rejection requires account repair.
* schema or account identity mismatch: disable automatic writes for that account and report the error.

After confirmed recovery:
* Read the updated quota windows and weekly deadline.
* Reconcile local quota cooldown for the affected account.
* Preserve disabled state, terminal authentication failures, unrelated failures, and newer errors.
* Invalidate relevant selection snapshots and reevaluate bindings at safe boundaries.
* Record before and after evidence.

Do not interrupt active inference to redeem a credit. Coordinate local scheduling with the account operation journal. Allow a safe reset to proceed while work continues only when the provider contract supports it. Verification must account for usage recorded after the reset; a successful reset does not require the next observation to show exactly zero usage. Record which covered windows were refreshed and which remain blocked.

The existing ResetQuota and /v8/management/routing/cooldown/reset clear local routing state. They do not redeem provider credits. Reuse them only when their broad reset semantics are safe. Otherwise add a narrow generation checked recovery operation.

Use account generation and observation timestamps to prevent an older recovery read from clearing a newer failure. Persist the result so saved cooldown state cannot restore the old block after restart.

For automatic or manual provider resets detected externally, perform the same recovery checks without submitting a consume request.

## 11. Required two account scenario

Use a fake clock and sanitized provider fixtures.

Initial state:
* Account A: weekly reset in four days; usable short and weekly windows; no expiring credit.
* Account B: weekly reset in seven days; usable short and weekly windows; one eligible credit expires in 24 hours.
* Both accounts belong to the requested pool.
* auto_expiring is enabled.

Expected behavior:
1. Prefer B for new and safely transferable work.
2. Keep A available for failover.
3. If B exhausts its short window while its weekly allowance remains, use A temporarily.
4. If B exhausts its weekly allowance and work is waiting, redeem B's selected credit.
5. If B does not exhaust weekly allowance, redeem by D when a covered window has usage to reset.
6. Reuse one request ID across an uncertain retry.
7. Confirm updated inventory and quota; reconcile B's local cooldown.
8. Recalculate priority from the observed reset times.
9. If B's next weekly reset is later than A's and B has no other expiring credit, prefer A for new work again.

Do not assume redemption always makes the next deadline exactly seven days from the write. Use observed provider times.

Variant tests must cover several credits, different expiry times, missing details, a nonexpiring credit, and an unsafe existing session pinned to A.

## 12. Dashboard, API, and operator controls

Extend the current implementation's existing manual reset expiry UI. Preserve its provider grouping, available reset entries, absolute times, and countdowns. Add the missing account ownership, routing explanation, automation settings, planned redemption time, and operation status in the existing view. Add a separate panel only if the current UI cannot support the required controls. Keep maintained source changes; do not patch only downloaded management HTML that automatic updates can replace.

Target UI requirements, implemented as additions or verified existing behavior:
* Alias, provider, plan when known, account status, and requested model eligibility.
* Short and weekly usage with absolute reset times.
* Preserve the existing Manual reset expiry list per provider. Make account ownership clear for every row, including when one provider has several accounts. Keep the list visible when automatic redemption is off.
* Available manual reset count and one row per known reset, with status, provider title, affected windows, and a Use reset action.
* Each manual reset's absolute expiry in the selected time zone and a live countdown such as Expires in 3h 14m.
* Separate Normal weekly reset and Manual reset expires labels. Never combine the two timers.
* Earliest expiry highlighted in the account list, with sorting by manual reset expiry.
* Warning states at 24 hours, one hour, and the configured expiry guard; preserve expired rows in the operation history.
* Explicit Does not expire for a confirmed nonexpiring reset and Expiry unknown when details are missing. Never show an invented countdown.
* Inventory completeness, last successful refresh, and stale evidence markers.
* Effective priority deadline and the reason for selection or exclusion.
* Active bindings, transport, snapshot age, and redemption state.

Verify the existing countdown implementation and extend it only where required. Update visible countdowns locally at least once per minute, and every second during the final minute. Timer ticks must not trigger provider requests. Recalculate from the absolute expiry after the page resumes or the clock changes. Show the planned automatic redemption time alongside the expiry when automation is enabled. Disable redemption for an expired or unavailable credit. After Use reset, show the operation state, refresh the inventory, and update the normal weekly timer from provider evidence.

Keep the current displayed GMT+2 label when it correctly represents the selected fixed offset. For Europe/Berlin mode, calculate the offset for each timestamp and show it correctly across daylight saving changes. Store the actual provider instant in UTC. Never change an expiry instant by reinterpreting its display label. The reported October dates cross Berlin's seasonal offset change, so verify this distinction with fixtures.

Controls:
* Enable policy; select automation mode; set fallback.
* Hold an account; set a reserve; select a credit; set or cancel a local schedule.
* Redeem now with clear account, credit, affected allowance, and timer effect.
* Clear local cooldown as a separate action.
* Refresh observations and export sanitized diagnostics.

Add proposed endpoints under /v8/management/account-policy:
* GET accounts
* GET decisions
* GET resets
* PATCH settings
* POST refresh
* POST resets/redeem
* POST resets/schedule
* DELETE resets/schedule/{schedule_id}

These are proposed contracts. Audit the current local endpoints and reuse equivalent contracts where they already exist. Add compatibility routes only when needed. All writes require management authentication. Responses must exclude credentials and message bodies.

Provide a small proxyctl command using these endpoints: accounts, explain, refresh, resets list, resets redeem, resets schedule, resets cancel. Use machine readable JSON output and stable error codes. The default reset command targets no account implicitly.

## 13. WebSocket and deployment requirements

Verify actual Codex client to proxy and proxy to provider transport. Enabling a credential flag alone does not prove both use WebSocket.

Test HTTP, SSE, and WebSocket through the same account policy. Cover connection reuse, normal completion, reconnect, compatible fallback, tool calls, compact requests, and continuation state. Preserve existing payload rule ordering.

Do not promise a fixed speed increase. Compare first token latency and completion behavior under a controlled workload.

Deploy one persistent proxy on Linux with service restart, durable state, protected credentials, and log rotation. Keep it available when the owner's laptop is closed.

Tailscale is the required access network. Deploy the central proxy on a persistent Linux host joined to the owner's tailnet. All remote Codex clients, agent workers, UI sessions, and proxyctl calls must use the configured tailnet endpoint.

Bind the proxy to its Tailscale interface, or bind to loopback and expose it through a private Tailscale forwarding service. Restrict tailnet access to approved devices and identities. Management access must be limited to the owner's approved operator devices. Keep client API keys and a separate management key.

Document the exact host name, port, inference base URL, management URL, listener configuration, and required tailnet access rules for the installed environment. Use one stable tailnet endpoint in client profiles and fleet configuration. Verify HTTP, SSE, and WebSocket access over Tailscale, including long running responses and reconnect behavior.

The proxy host maintains provider connections and account state. Client disconnection from Tailscale must not stop the proxy service, discovery service, or reset scheduler. A remote worker must continue its task when the owner's UI device disconnects. Preserve request correctness when a worker's own network connection fails.

Do not expose inference or management through public port forwarding or a public sharing service. The intended deployment is private access over Tailscale. Report tailnet connection failures separately from upstream provider, authentication, and quota failures.

Document isolated Codex profiles for direct access and proxy access. Do not replace the owner's normal profile without an explicit migration step.

## 14. Companion agent workflow

Deliver this separately from the proxy module:

* A fleet configuration repository with the stable Tailscale proxy endpoint, worker identity, required tools, and service recipes. Keep secrets outside git.
* Remote agent processes that survive client disconnects. Workers connect to the central proxy through the owner's tailnet.
* Worker selection by availability and capacity. Keep machine scheduling separate from account scheduling.
* A task inbox with working, needs_input, review_ready, failed, and settled states. Add archive, snooze, and search through the existing agent tool where available.
* Pending messages delivered at a safe agent tool boundary; an explicit steer action uses the agent tool's supported interrupt mechanism.
* One worktree or isolated checkout per implementation task.
* Separate implementation and review contexts.
* A bounded approved backlog for overnight work. No automatic meaningless token consumption.
* Local build concurrency limits and an option to send CI to another worker.
* Reviewable PRs, runnable previews when needed, CI repair, and a documented rollback.

The proxy may export deadline and availability information to this companion. It must not become a full agent host, GitHub automation platform, or conversation inbox.

## 15. Work packages for implementation agents

The lead agent freezes the data contracts before parallel implementation. Use separate worktrees. Do not assign concurrent edits to the same shared files.

| Package | Ownership and deliverable | Dependencies |
| --- | --- | --- |
| A | Audit existing local reset UI and data flow, fixtures, identity mapping, reuse or complete Codex adapter | None |
| B | Persistent snapshots, discovery service, configuration, operation journal | A contracts |
| C | Deadline selection, reserves, affinity composition, explain output | B contracts |
| D | Redemption state machine, account locking, recovery reconciliation | A and B |
| E | Reuse or extend management API, proxyctl, existing manual reset UI and policy controls | B, C, D contracts |
| F | Transport integration, service deployment, companion workflow, final review | Integrated A to E |

Recommended new Go module area: internal/accountpolicy, split into model, codex client, observer, policy, reset scheduler, journal, and recovery files. Follow current repository conventions.

Expected existing integration areas:
* sdk/cliproxy/auth/selector.go and conductor_selection.go
* sdk/cliproxy/auth/conductor_cooldown.go and types.go
* sdk/cliproxy/auth/scheduler.go
* internal/config/config_types.go and configuration loading
* internal/api/handlers/management and server_management_v8.go
* sdk/cliproxy/service lifecycle
* Existing Codex executor helpers and quota signals

These are integration targets, not instructions to edit every file.

Retain an adapter boundary for a later scheduler plugin. Current plugin interfaces support scheduler candidates, affinity lookup, and local cooldown reset. Audit ordering before relying on a plugin to override an established binding.

Reject conflicting ownership at startup. If CLIProxyAPIHome or another scheduler owns account selection or cooldown state, require an explicit supported integration. Do not silently run two policies.

Each agent returns changed files, contract changes, commands run, results, remaining limits, and rollback steps. The lead agent checks the complete two account scenario after integration.

## 16. Acceptance and release gates

Required deterministic tests:
* A resets in four days, B in seven days, no credit: new work chooses A.
* B has a credit expiring tomorrow: new eligible work chooses B.
* Short exhaustion, weekly exhaustion, and model specific blocks select correctly.
* Missing or stale evidence uses the documented fallback.
* Credit count without details never invents expiry.
* Nonexpiring credits remain saved in auto_expiring mode.
* Concurrent requests cause one logical redemption.
* Restart and lost responses reuse the saved request ID.
* reset, already_redeemed, nothing_to_reset, and no_credit are distinct.
* The earliest selected credit is redeemed; inventory conflicts are handled.
* A three credit fixture based on the reported expiry order prioritizes the credit expiring first and preserves the other two. Use a frozen clock and full provider timestamps.
* The existing manual reset list remains present with no duplicate rows or duplicate discovery polling.
* Provider grouping cannot route or redeem a credit against another account.
* Routing consumes structured credit IDs and expiry instants, not rendered labels or countdown strings.
* Newer failures survive older recovery evidence.
* Persisted quota cooldown is reconciled after confirmed recovery.
* Strict bindings, safe migration, unsafe continuation, and child sessions behave as specified.
* HTTP, SSE, and WebSocket apply the same eligibility rules.
* Disabling the module restores existing routing behavior.
* Account or workspace mismatch prevents reset writes.
* Manual weekly resets are visible with automation disabled.
* Each known expiry has an accurate local countdown and an absolute time.
* Normal weekly renewal, manual reset expiry, and scheduled redemption have separate labels.
* Unknown expiry, nonexpiring credit, expired credit, stale inventory, and successful redemption have distinct UI states.
* UI countdown updates make no provider requests and recover correctly after page suspension.
* An authorized device can use inference, the management UI, and proxyctl through the configured Tailscale endpoint.
* HTTP, SSE, and WebSocket work over the intended Tailscale deployment.
* Tailnet rules exclude unauthorized devices and limit management to approved operators.
* The proxy has no unintended public listener or public forwarding path.
* Disconnecting the owner's UI device leaves remote workers, discovery, and reset scheduling running.
* Expiry scheduling works across clock changes, UTC offsets, and Berlin daylight saving changes.
* Logs and diagnostics contain no credentials or prompt bodies.

Use fake clocks. Do not use wall clock sleeps to test deadlines.

Run the relevant package tests, race tests, repository test suite, formatting, and the required compile check. Use the Go version required by the chosen repository base. At the inspected base it is Go 1.26.

No live credit redemption is needed for CI. Validate against mock provider fixtures. Keep any optional live smoke test explicit, account scoped, and separate from ordinary test runs.

Release in stages: observation, routing, reset dry run, then enabled automatic redemption. Include a feature switch and a rollback recipe that preserves pending operations and account credentials.

Completion requires working tests, complete operator documentation, the scenario in section 11, and a clear report of any unsupported backend or client behavior.

## 17. Source references

CLIProxyAPI source at the inspected commit:
https://github.com/router-for-me/CLIProxyAPI/tree/8ef43e4df3b216a42493105d31c2873b69191473

Read:
* AGENTS.md
* config.example.yaml
* sdk/cliproxy/auth/selector.go
* sdk/cliproxy/auth/quota_signals.go
* sdk/cliproxy/auth/types.go
* internal/runtime/executor/helps/codex_quota.go
* internal/runtime/executor/codex_websockets_executor.go
* internal/pluginhost/routing_callbacks.go
* sdk/pluginapi/types.go
* docs/management-api-v8.md
* internal/managementasset/updater.go
* examples/plugin/scheduler

Related discussions:
* https://github.com/router-for-me/CLIProxyAPI/issues/5782
* https://github.com/router-for-me/CLIProxyAPI/issues/5800
* https://github.com/router-for-me/CLIProxyAPI/issues/5639
* https://github.com/router-for-me/CLIProxyAPI/issues/6274

5782 concerns routing toward more remaining capacity. This specification prioritizes expiring allowance and reset opportunities. 5639 concerns stale local cooldown after provider recovery. 6274 supplies a local recovery callback, not provider redemption.

Official Codex source at the inspected commit:
https://github.com/openai/codex/tree/afb436df8b70bb5bc57b86d9a3e829968988cd21

Read:
* codex-rs/backend-client/src/client/rate_limit_resets.rs
* codex-rs/backend-client/src/types.rs
* codex-rs/app-server/src/request_processors/account_processor/rate_limit_resets.rs
* codex-rs/app-server-protocol/src/protocol/v2/account.rs
* codex-rs/app-server-protocol/schema/json/ClientRequest.json
* codex-rs/app-server/tests/suite/v2/rate_limit_reset_credits.rs
* codex-rs/tui/src/chatwidget/reset_credits.rs

Provider explanation:
https://help.openai.com/en/articles/20001498-how-banked-codex-resets-work

Secondary implementation reference:
https://github.com/xiaocoss/quotio-desktop/blob/21d75d08b38a23f97fbd534a1768a829cb147f2c/crates/quotio-core/src/quota.rs

Quotio demonstrates redemption, but the official Codex detail and outcome types are the stronger reference for expiry and selected credit handling.

User input:
Pasted text(20261004-143754).txt, complete 2002 line video transcript, plus the owner's two account routing and expiring reset requirement. The owner also reports that the current local implementation already displays three manual reset entries with absolute expiry and countdowns per provider.
