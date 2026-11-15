# Daily upstream watch

A scheduled agent run (T3 scheduled task, daily 04:00 Europe/Berlin) follows this
runbook. It checks router-for-me/CLIProxyAPI (`upstream`) for fixes worth taking,
and deploys only within the authority below. When this runbook and a request in
the run's prompt disagree, the stricter rule wins.

State lives in `/DATA/AppData/CLIProxyAPI/.tmp/upstream-watch/state.json` (not
in Git). Each review is recorded in `docs/integration/upstream-review-YYYYMMDD.md`
(gitignored; commit it with `git add -f`).

## Authority (granted by the operator on 2026-11-15)

The run may deploy without asking only a change that meets all four conditions:

1. **Class A or class B.**
   - Class A: a fix for a published upstream security advisory (GHSA) rated high
     or critical, on a code path production actually uses.
   - Class B: a bug fix where production logs from the last 48 h show the failure
     happening on a provider or endpoint production uses. Quote the log evidence:
     counts, statuses and timestamps from the API RESPONSE `Status:` lines or the
     `gin_logger` status column, never from text quoted inside request bodies.
2. **Not routing-adjacent.** The ported diff touches none of the protected paths
   below.
3. **Gates pass.** Every test gate below passes on the exact deploy commit.
4. **Codex approves.** An independent Codex review approves the exact deploy commit.

Anything else is reported for the operator's approval. That includes routing-adjacent
security fixes, performance and routing improvements, and features. The run may
prepare and test a branch for it, but must not deploy it.

The only upstream changes worth integrating are real benefits: security, routing,
performance, or reliability on paths that are actually used. Production works, so
"newer" is never a reason. Skip any change that pins or narrows credential
selection: it blocks failover away from exhausted accounts.

## Protected paths (hard block on auto-deploy)

If the port, or anything it pulls in, changes any of these paths, the change can
only be proposed:

- `sdk/cliproxy/auth/**` (selector, conductor, cooldown, scheduler, leases, account policy)
- `internal/accountpolicy*/**`, `sdk/cliproxy/service_account_policy.go`,
  `internal/api/server_account_policy.go`, `internal/api/handlers/management/account_policy*.go`
- `internal/config/**`, `sdk/config/**`, `config.example.yaml` (defaults)
- `internal/registry/**` (model and provider resolution feeds routing)
- `internal/store/**`, `internal/auth/**`, `sdk/auth/**`, `internal/watcher/**` (credential identity, refresh and synthesis)
- `ops/idle-deploy/**` (the deploy runner itself)
- `go.mod` / `go.sum`, unless the fix itself is a dependency bump that resolves the advisory

In every case, auto-deploys never edit production `config.yaml`, `auths/`, policy
state, journals, bindings, schedules or pending operation UUIDs, and never use
`prepare --config`. Automatic reset redemption stays enabled; never run a live
redemption.

## Procedure

0. **Preconditions.** If any precondition fails, report and stop.
   - Take `flock -n .tmp/upstream-watch/run.lock`. If it is held, another run is
     active.
   - Production is healthy: one `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8317/healthz`.
     Never loop an unauthenticated management request: five failed management
     auths ban the Docker gateway IP.
   - Find the served commit with `docker inspect cli-proxy-api --format '{{.Image}}'`,
     matched against the image tags of `docker images`, or with a single
     `curl -sI .../v8/management/config | grep -i x-cpa-commit`.
1. **Collect.** Run `git fetch upstream --tags` and `git fetch origin`.
   - List the commits in `last_reviewed_upstream..upstream/main`, plus releases.
   - List advisories:
     `gh api repos/router-for-me/CLIProxyAPI/security-advisories --jq '.[]|[.ghsa_id,.severity,.published_at,.summary]|@tsv'`.
   - If there are no new commits and no new advisories, post a one-line report
     and stop.
2. **Triage.** Production profile: Claude OAuth, Codex OAuth and the OpenCode Go
   plugin provider. Clients are Claude Code on `/v1/messages` and Codex/pi on
   `/v1/responses` (gpt-* models).
   - Gemini, Antigravity, Devin, Vertex, Home, SIWC and Muse are unused.
   - Drop anything already in the fork. Search `git log --all --grep <sha>` and
     `docs/integration/`.
   - Check each candidate against `logs/main.log` (root-owned; `sudo -n`; covers
     about 30 h) and against the request logs (about 1 h). Grep with `-F`/`-l` under
     `timeout`; `.{0,N}` regexes hang on multi-MB lines.
3. **Port.**
   - Create a worktree at `.worktrees/upstream-YYYYMMDD` from the served commit.
   - Cherry-pick with `git cherry-pick -x`, together with the fix's
     prerequisites, follow-ups and tests.
   - Upstream uses MoeForge/moego `ojson`; the fork uses tidwall gjson/sjson.
     Translate every ojson hunk.
   - Resolve conflicts by hand using `docs/account-policy-upstream-updates.md`;
     never with a blanket `--ours` or `--theirs`.
   - Commit identity: `-c user.name=opsec -c user.email=136596632+levi-cm@users.noreply.github.com`.
4. **Gates.** Use `GO_BINARY=/DATA/AppData/CLIProxyAPI/.tools/cliproxy-repair-toolchain/go/bin/go`
   and `TMPDIR=/DATA/AppData/CLIProxyAPI/.tmp`, and run Go only through
   `scripts/go-limited.sh`. All of these must pass:
   - `gofmt -l` (the toolchain's `gofmt`) and `git diff --check`
   - `go test ./...`
   - `go test -race ./internal/accountpolicybindings ./internal/accountpolicyclient`
   - `go test -race ./internal/accountpolicy ./internal/accountpolicyusage ./internal/accountpolicyui ./internal/api ./internal/api/handlers/management ./sdk/cliproxy ./sdk/cliproxy/auth ./sdk/cliproxy/usage ./cmd/proxyctl ./cmd/account-policy-preview`
   - `node --test internal/accountpolicyui/*test.cjs`
   - `node --test client-usage/codex-auth-headers.test.mjs`
   - `python3 -m unittest discover -s companion`
   - `go build ./cmd/server`
5. **Independent review.** Start a new `delegate_task` on the Codex provider for
   every round. Brief it with:
   - the upstream commit(s) and the ported diff;
   - the class, the evidence, and the protected-path rule;
   - an instruction to check that routing, cooldowns, OAuth identities,
     conversation ownership, leases, redemption, journals, pending UUIDs, config
     and defaults are unchanged.

   Fix what it finds, re-run the gates, and start a fresh review. After three
   rounds without approval, report a blocker and do not deploy.
6. **Deploy (class A or B only, with every condition met).**
   - **Deploy commit:** the served commit, plus the reviewed fix, plus only the
     commits listed in `state.json` `approved_to_ride`. Never ship anything in
     `pending_unapproved`. If the production branch `feat/usage-split-scheduler`
     carries unapproved commits, build from the worktree above instead: the
     served commit plus cherry-picks. Afterwards, rebase the pending commits onto
     the new production commit for later approval.
   - **Single flight:** no `cliproxy-idle-deploy-*` or `cliproxy-idle-watchdog-*`
     unit may be active, and no release may be waiting. Never stop or replace a
     release someone else launched. If one exists, report it and stop.
   - **Build** as a `systemd-run` unit, because unused images may be pruned and T3
     restarts kill background shells:
     `docker build -t cli-proxyapi-fork:upstream-<sha8> --label org.opencontainers.image.revision=<sha> --build-arg COMMIT=<sha> --build-arg VERSION=<ver> --build-arg BUILD_DATE=<iso> .`
   - **Prepare:** `sudo -n python3 ops/idle-deploy/main.py prepare --image ... --commit <full sha> --retire-idle-clients`.
     Run it from `.worktrees/usage-split-20261111`. It must pass, including the
     compose dry run. Do not use `--config`, and do not use an outage budget above 40.
   - **Launch:** `sudo -n python3 ops/idle-deploy/main.py launch --now --release .../release.json`.
     `--now` skips only the five-minute quiet period. It still requires zero
     active requests and zero non-dormant connections, so it never cuts an active
     request. Never use `interrupt.py`, which needs the operator. Never run
     `docker compose up/restart/stop/rm` against production.
   - **Recovery:** the runner is a detached systemd unit. It starts the router,
     then requires State `running`, `/healthz` 200 and an authenticated
     `/v1/models` 200 within about 60 s. Otherwise it restores the previous image,
     `.env` pin and `config.yaml`, verifies them the same way, and keeps retrying
     through an independent rescue unit until something serves. This agent
     loses its model connection during the cutover, and the runner does not
     depend on it.
   - **Outcome:** read only from
     `output/verification/idle-deploy/RELEASE/status.json` (`deployed`,
     `rolled_back`, `original_retained`, ...). Never report success without
     `deployed` and its host verification. If the run ends before the outcome is
     known, the next run reports it.
7. **Record.**
   - Write `docs/integration/upstream-review-YYYYMMDD.md`: decisions and evidence
     per commit, and the gates and review verdicts.
   - Update `state.json`. Set `last_reviewed_upstream` only after triage is
     complete. Add a deploy record when a deploy happens.
   - After a deploy, update the production memory note
     (`production-branch-and-deploy`). Do not push unless the operator asks.
8. **Report.** End with a short report:
   - what is new upstream and what was taken, and why;
   - what was deployed, with its `status.json` outcome;
   - what waits for the operator, with a recommendation;
   - watch items.
