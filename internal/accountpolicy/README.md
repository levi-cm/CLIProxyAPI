# Optional account policy

This module owns provider observations, deadline evaluation, reset-credit
selection and the durable redemption state machine. It imports neither the
proxy config package nor SDK auth/API code. Integration adapters are separate
`account_policy_*.go` files in the SDK and API; see
[the integration map and update procedure](../../docs/account-policy-upstream-updates.md).

## Immediate exhaustion rule

With policy enabled and `automation: auto_expiring`, fresh ordinary **weekly**
exhaustion immediately selects the earliest eligible credit whose expiry is
strictly before the next normal weekly refresh. It requires neither queued work
nor proximity to expiry. A credit with 11 hours remaining is spent now if weekly
allowance is exhausted and normal renewal occurs after that expiry. Credits
expiring at or after normal renewal stay saved. Short-window denial alone does
not establish weekly exhaustion; provider `allowed` is an aggregate scope flag,
not evidence that both windows are exhausted. Explicit permission also prevents
interpreting a rounded displayed 100% as exhaustion.

The expiry-minus-guard safety deadline remains an independent fallback when
covered allowance has already been used but is not exhausted. Nonexpiring
credits are excluded from `auto_expiring`; `auto_all_saved` is a separate explicit
authorization with demand, complete-inventory and saved-credit reserve rules.
`off`/`notify` never automatically redeem. An explicit operator schedule or
selected manual redemption is distinct from automatic mode.

All writes still require verified account/workspace ownership, supported type
and scope, unexpired known credit details, fresh usage and inventory, durable
journal availability, and an idle account reservation. Holds stop automatic
redemption; read-only mode stops provider writes. Unknown expiry, purchased
balances and unsupported classes never become guessed expiring resets.

## Success and recovery

Journal the selected credit and request UUID before submission. Uncertain
responses retain that UUID; read-only discovery can reconcile a consumed credit
and recovered allowance without submitting again. Distinguish `reset`,
`already_redeemed`, `nothing_to_reset` and `no_credit`. Repeated unchanged evidence
must not create new logical operations. Schedules retain their original upstream
owner so credential re-registration cannot redirect an approved write.

After redemption, reread usage and selected inventory, verify allowance recovery
and credit consumption, and publish the new weekly refresh time. The SDK recovery
adapter clears only affected older quota cooldowns and persists repairs before
publishing. Newer failures, authentication blocks and unrelated model cooldowns
survive. Routing evaluates the refreshed snapshot on the next selection; it does
not splice an in-flight stream or move an unsafe conversation continuation.

Use fake clocks in tests. Relevant passive quota events request a coalesced fresh
provider observation; successful requests that merely advance SDK result
generation must not trigger per-request discovery polling. Native lifecycle
continues independently of the browser. No live provider writes are needed to
run the regression suite.

## State and rollout

Use one protected local state directory and writer instance. Journal locking
fails closed on unsupported platforms. Disable the policy and automation in YAML
for rollback, retaining credentials and pending operation state. YAML disabled/
off is the restart authorization ceiling for persisted operator settings.
Malformed contract/identity failures inhibit automatic writes while preserving
uncertain operations for read reconciliation; repair the provider contract and
review pending state before explicitly restoring write authority.

Deployment, tailnet/operator boundaries and staged activation are documented in
[the deployment guide](../../docs/account-policy-deployment.md). Production
services and remote-device ACL verification are operator provisioning tasks, not
claims made by the fixture tests.
