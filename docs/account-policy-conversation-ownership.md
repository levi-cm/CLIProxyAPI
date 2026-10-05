# Durable conversation ownership

With account-policy routing enabled, successful Codex requests with an explicit
session identity save their owner in `conversation-bindings.json` inside the
existing `account-policy-state` volume. No additional disk or Docker volume is
needed. The ordinary one-hour routing cache can expire without losing that
evidence. Read-only provider/reset mode still permits local ownership observations.

## Validation and storage

Ownership is scoped to the authenticated inference caller, provider, canonical
session and canonical routing model. Caller scope and session are stored as
SHA-256 digests; raw inference keys, prompts, reasoning and OAuth tokens are never
stored here. Account and workspace IDs are private operational metadata.

Restoration requires the exact registered credential ID, provider, account and
workspace to agree with the saved owner and provider observation identity. The
shared credential validator compares trusted local credential metadata and
decoded OAuth account claims when present; it is not an independent JWT signature
verifier. Refreshed tokens and new process-local registration epochs do not
invalidate the same logical owner. Credential reassignment, conflicting claims,
account holds, model restrictions and quota cooldowns cannot be bypassed.

A fork or spawned agent can inherit only an explicitly supplied parent's exact
caller/provider/model binding after the same validation. Its own successful
request establishes an independent child binding. A known child's own owner
takes precedence over later changes to the parent. No cross-model or global
"last used account" fallback is used. Changing the inference key changes caller
scope, and may therefore require a new conversation.

Successful completion rechecks the selected credential's logical identity and
registration epoch. Failed attempts and token counting do not establish owners.
Atomic private-file replacement, file/directory sync, a cross-process write lock
and ownership compare-and-swap prevent partial files, lost concurrent records
and obsolete completions replacing a newer owner. A stale local cache conflicting
with durable evidence fails closed rather than guessing which state is safe.

The state directory and its parents must be real directories on a trusted local
filesystem, with permissions and ownership that prevent untrusted writes. State
and lock files are mode 0600; symlinked, non-private, malformed, oversized or
unsupported state is rejected. Writes require supported Unix process locking;
this deployment is Linux. The store holds at most 65,536 records / 64 MiB, without
automatic eviction of established owners. A full or unwritable store logs a
sanitized persistence error; an unrecorded completion cannot safely resume after
restart. This is not a multi-replica dispatch coordinator: deploy one active proxy
per conversation namespace. Atomic locking protects files, not concurrent first
assignment across separate proxy runtimes.

## Recovering a 409

- `account_policy_owner_unknown`: no exact saved/live owner exists. Repeated
  retries, selecting the account with the earliest refresh, or changing routing
  mode cannot establish where encrypted state originated.
- `account_policy_owner_mismatch` / `unsafe_affinity_unavailable`: the saved owner
  changed or is unavailable. Restore the original credential identity or resolve
  its hold/quota/model availability; do not assign the old conversation to a
  replacement account.
- `account_policy_owner_conflict`: live and durable evidence disagree. Finish any
  active work and investigate the writers. Do not erase ownership to force routing.
- `account_policy_binding_state_unavailable` (503): repair permissions, storage or
  a corrupt state file using a verified backup. Requests do not fall back to a
  guessed owner when the journal cannot be trusted.

For bindings already lost **before** this feature was deployed, use a new Codex
conversation containing a plain-text handoff, without old encrypted reasoning,
response IDs or compaction items. Alternatively restore an authoritative backup
of this exact caller/session/model ownership and original credential identity.
Usage totals, account emails and recent selection alone are not ownership proof.
There is no unauthenticated import endpoint or UI button for guessing an owner.
If the old runtime still has a valid live binding, its next successful request can
save it; deploying/restarting before that request cannot reconstruct lost evidence.

Back up this file together with the rest of the private policy state before an
update. Preserve it on rollback: earlier versions simply do not read it. Never
commit the state directory or credential files.

## Upstream hooks and verification

Store logic is isolated in `internal/accountpolicybindings/`; restoration and
completion logic is in `sdk/cliproxy/auth/account_policy_bindings.go`. The existing
policy selector has small prepare/capture/success hooks and two optional fields.
`service_account_policy.go` provides the manager's fresh credential resolver.
No payload-building, translator, upstream selector interface or v0 management
endpoint changes are required. Merge upstream using
[the fork update procedure](account-policy-upstream-updates.md).

```bash
go test ./internal/accountpolicybindings ./sdk/cliproxy/auth ./internal/api
go test -race ./internal/accountpolicybindings ./sdk/cliproxy/auth ./internal/api
go test ./...
go build -o test-output ./cmd/server
```

Restart regression coverage exercises real HTTP/SSE handlers and production
Codex executors, encrypted resume, parent inheritance, child restart, unknown
owner rejection before upstream I/O, changed identities and caller/model isolation.
The live verification helper uses actual provider encrypted reasoning through a
temporary loopback-only SOCKS5 relay limited to this proxy. It proves server-side
SOCKS transport, not a remote client's unrelated SOCKS tunnel. On the affected
device, test a fresh conversation, resume it after a proxy restart, and spawn a
child with the same model through its normal SOCKS configuration. Old lost
conversations still require the recovery choices above.
