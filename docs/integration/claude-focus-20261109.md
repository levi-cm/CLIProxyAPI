# Integration 2026-11-09: Claude focus mode, pending release and upstream fixes

Branch `integration/claude-focus-20261109` (worktree
`.worktrees/claude-focus-20261109`) starts at `7464c451`, the application of the
never-deployed idle release `6b098664c3` (credit reserve, history startup repair,
idle-deploy fixture fix on top of selective #7256-#7271). Production served
`bc4ad493` before this integration.

## Contents

| Commits | Purpose |
| --- | --- |
| `884db61c`, `bb7a6eef`, `6a098c28`, `708f2eb2`, `6c1979ee` | Verified selective batch #7289/#7294/#7300/#7279 from `integration/selective-7289-7317-20261107`, without upstream `68cd7572` (durable Codex refresh journal), so rollback to `bc4ad493` stays protocol-safe |
| `4e38c482` | `ops/idle-deploy/interrupt.py`: operator-authorized cutover that interrupts in-flight traffic instead of waiting for an idle window |
| `b69f0c24` | Claude weekly-deadline routing and focus mode (see `internal/accountpolicy/README.md`) |
| `67fce362` | upstream `9037d543` (#7146) drop output-only `content_token_count` from replayed Responses WebSocket items |
| `8e57d8c3` | upstream `0130d6f3` (#7197) keep Fable-only 429s model-scoped |
| `f56bde89` | upstream `e5827065` (#7183) stop auto-refresh spin for disabled credentials |
| `87e71051` | upstream `9d08f0d4` (#6936) strip credential headers on cross-origin plugin redirects |
| `9e159850` | upstream `f4d3f061` (#7320) replace WebSocket transcript when tool calls are unanswered; `ojson` translated to `gjson` |
| `20e24d35` | upstream `ce38c75c` (#7326) drain Claude stream after `message_stop` for connection reuse; `ojson` translated, fork-absent usage observer lines dropped |
| `ce2a2789`, `64edab12` | upstream `04208efa` (#7225) preserve disabled plugin auth state; only `preserveAuthDisabledState` and its regression test |

Upstream candidates were triaged against `upstream/main` `681ce41b`. Deferred:
`05e668cf` (#7341, needs a full gjson rewrite), `374056f2`, `dde469d3`,
`e458999b`, `1d45a5f3` and the SIWC chain that builds on `68cd7572`.

## Claude cross-account evidence

Verified on 2026-11-09 from Anthropic documentation (preserved thinking,
prompt caching, Files API, code execution), community reports, and production
logs. Opus 5.5 and Fable 5.1 thinking remains valid on another account; the cost
of a move is one prompt-cache rebuild (observed 119k and 256k cache-write tokens).
Sonnet 5.5 and Haiku 5.5 thinking is account-bound and silently dropped
(`organization_binding_mismatch`). Codex encrypted reasoning is login-bound and
stays pinned.

## Verification

All Go commands used `scripts/go-limited.sh`. Evidence:
`output/verification/claude-focus-20261109/` in the project root.

- Full `go test -count=1 ./...` on `b69f0c24`: 111 packages passed, 0 failed;
  server build passed.
- Final head: server build, `go vet` and tests of every package touched by the
  upstream picks, plus race tests for `sdk/cliproxy/auth` and
  `internal/accountpolicy` (`final-checks.log`): build, vet, 20 packages and
  race all passed.
- Deployment controller: 104 Python tests passed (16 new for interrupt mode).
