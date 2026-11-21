# Recurring task: merge and test upstream CLIProxyAPI

Run this task only when explicitly asked to execute it. Creating or reading this
file does not start an update or create a schedule.

## Goal

Merge all changes from the latest upstream `main` into this fork, preserve the
fork's custom functionality, resolve integration problems, and verify the result
with tests and a server build.

- Original repository: https://github.com/router-for-me/CLIProxyAPI
- Fork: https://github.com/levi-cm/CLIProxyAPI
- Upstream update source: `upstream/main`, not experimental `dev`.

## Preparation

1. Read the current `AGENTS.md`,
   [fork integration guide](docs/account-policy-upstream-updates.md), and
   [conversation ownership guide](docs/account-policy-conversation-ownership.md).
   Follow their current repository conventions and integration requirements.
2. Inspect the branch, working tree, remotes, and any in-progress merge. Preserve
   unrelated edits and untracked files. Never discard them or use a blanket stash.
3. Verify that `upstream` points to the original repository above. Add it if
   missing; investigate a different URL before fetching. Fetch upstream branches
   and tags, determine its current default branch, and record the exact upstream
   commit being integrated. If the default branch changed from `main`, verify the
   change before selecting the new branch.
4. Record the fork's starting commit and create a local recovery ref. Use a
   uniquely named update branch/worktree from the current fork development tip
   when the working checkout has unrelated edits. Keep private configuration,
   credentials, policy journals, conversation bindings, and nested repositories
   outside the update. Tests must use fixtures and temporary state.
5. Read the incoming commit log and diff to understand changed interfaces and
   behavior. Merge the complete fetched upstream tip; do not cherry-pick only
   selected upstream changes. If it is already included, report that and verify
   the existing integration without creating an empty merge.

## Merge and resolve

1. Merge the selected upstream commit with `git merge --no-ff --no-commit` in the
   prepared checkout. Resolve conflicts individually, retaining new upstream
   behavior and the fork's optional extension hooks. Do not resolve entire
   integration files with blanket `--ours` or `--theirs`.
2. Preserve account policy, quota observation, reset safeguards, scheduling,
   dashboard controls, client usage/MCP integration, and durable conversation
   ownership. Keep the independent Telegram companion separate and validate its
   API contracts if those change.
3. Adapt custom code to upstream interface changes. Preserve credential mutation
   locking and persistence, stream/request lease lifetimes, caller/session/model
   isolation, encrypted continuation and parent/child ownership, and disabled
   policy behavior. Unknown encrypted owners must still fail closed.
4. Preserve the canonical thinking pipeline and the executor rule that user
   payload configuration is applied exactly once as the final semantic barrier.
   Add regression tests for changed request-building paths. Follow the translator,
   executor helper, timeout, and deprecated management endpoint rules in
   `AGENTS.md`. Check CLIProxyAPIHome compatibility when affected.
5. Fix regressions introduced by the merge with small, focused changes and
   meaningful tests. Avoid unrelated refactoring or replacing newer upstream
   request-building code with older fork code.

## Validation

Use the Go version required by the resulting `go.mod` (currently Go 1.26+).
If Go is unavailable on the host, use a suitable isolated build environment.
Do not skip required checks because a tool is missing.

Run the current integration guide's checks, including:

```bash
gofmt -w .
git diff --check
go test ./...
go test -race ./internal/accountpolicybindings ./internal/accountpolicyclient
go test -race ./internal/accountpolicy ./internal/accountpolicyusage ./internal/accountpolicyui ./internal/api ./internal/api/handlers/management ./sdk/cliproxy ./sdk/cliproxy/auth ./sdk/cliproxy/usage ./cmd/proxyctl ./cmd/account-policy-preview
node --test internal/accountpolicyui/*test.cjs
node --test client-usage/codex-auth-headers.test.mjs
python3 -m unittest discover -s companion -v
go build -o test-output ./cmd/server && rm test-output
```

Also run focused regressions for affected features, especially confirmed reset
and companion routes, credential recovery/locking, payload ordering, and
conversation ownership. Verify fresh Responses requests and encrypted
continuations, persistence across restart, child inheritance, and rejection of
unknown or mismatched owners through fixture HTTP/SSE tests. A health endpoint or
a successful fresh request alone does not prove continuation works.

Repeat the integration guide's fixture preview/browser checks when UI or API
contracts change. Keep previews separate from the running production service.
Report every failing, skipped, or unavailable check with its reason. Investigate
failures and fix merge regressions before calling the update ready.

## Finish and report

1. Review the final diff and confirm no conflict markers, secrets, generated
   binaries, or unrelated edits are included. Update the integration guide's
   reviewed upstream commit and any changed hooks or validation requirements.
2. Commit the tested merge locally, staging only the intended integration paths.
   If an isolated worktree was used, integrate the tested commit into the original
   fork branch when this can preserve all existing work; otherwise leave the
   reviewed update branch ready and explain the exact integration blocker.
3. Confirm the resulting commit contains the selected upstream commit in its
   ancestry. Report the upstream commit/version, resulting fork commit/branch,
   conflict resolutions, compatibility changes, test/build results, and any
   remaining blockers.

This task covers source integration and validation. Publishing to GitHub and
rebuilding, restarting, or deploying the production container require a separate
request. Report which version is tested without claiming the running container
has been updated.
