# Daily upstream and model watch

A scheduled agent run (T3 scheduled task, daily 04:00 Europe/Berlin) follows this
runbook. It does two things:

- It checks router-for-me/CLIProxyAPI (`upstream`) for fixes worth taking.
- It checks whether new Codex or Claude models need anything to work for
  production clients.

It deploys only within the authority below. When this runbook and a request in
the run's prompt disagree, the stricter rule wins.

State lives in `/DATA/AppData/CLIProxyAPI/.tmp/upstream-watch/state.json` (not
in Git). Each review is recorded in `docs/integration/upstream-review-YYYYMMDD.md`
(gitignored; commit it with `git add -f`).

## Authority

Granted by the operator on 2026-11-15 (classes A/B) and extended on 2026-11-18
(class M, advisor switch, interrupting launches).

The run may deploy without asking only a change that meets all four conditions:

1. **Class A, B or M, or an advisor switch.**
   - Class A: a fix for a published upstream security advisory (GHSA) rated high
     or critical, on a code path production actually uses.
   - Class B: a bug fix where production logs from the last 48 h show the failure
     happening on a provider or endpoint production uses. Quote the log evidence:
     counts, statuses and timestamps from the API RESPONSE `Status:` lines or the
     `gin_logger` status column, never from text quoted inside request bodies.
   - Class M (model enablement): an upstream fix needed for a Codex or Claude model
     that the official routing catalog lists. A real test request for that model
     must fail on the served commit (record its status and error type), and the
     upstream change must target exactly that failure: commit message, linked
     issue and tests.
   - Advisor switch: see "Advisor switch" below. It is a config-only change.
2. **Not routing-adjacent.**
   - The ported diff touches none of the protected paths below.
   - Upstream model-support commits often also change the embedded catalogs
     (`internal/registry/models/*.json`) and `cmd/fetch_*`. Drop those data-only
     hunks from a class M port, because the remote catalog supplies them at
     runtime. If the fix itself needs a protected path, it can only be proposed.
3. **Gates pass.**
   - A code change must pass every test gate below on the exact deploy commit.
   - An advisor switch must pass the config check under "Advisor switch".
4. **Review approves.**
   - An independent Codex review approves the exact deploy commit or config.
   - An interrupting launch also needs the multi-family review (step 7).

Anything else is reported for the operator's approval. That includes
routing-adjacent security fixes, performance and routing improvements, and
features. The run may prepare and test a branch for it, but must not deploy it.

Apart from class M, the only upstream changes worth integrating are real
benefits: security, routing, performance, or reliability on paths that are
actually used. Production works, so "newer" is never a reason. Skip any change
that pins or narrows credential selection: it blocks failover away from exhausted
accounts.

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

In every case, auto-deploys never edit `auths/`, policy state, journals,
bindings, schedules or pending operation UUIDs. Production `config.yaml` changes
only through `prepare --config`, and only for an advisor switch whose staged file
differs from the live file in `subagent-advisor.models` alone. Automatic reset
redemption stays enabled; never run a live redemption.

## Model check (no restart needed; every run)

Use the router API key from `/DATA/AppData/CLIProxyAPI/.env` (`CLI_PROXY_API_KEY`).
Read it into a shell variable, never print it, and never put it in a delegate
brief.

1. **Routable models.** Compare against `state.json` `known_models`; anything new
   is a new model.
   - Codex: one authenticated `GET /v1/models` (Bearer) lists the `gpt-*` IDs.
   - Claude: Claude IDs appear only in the Claude-format listing, so make one
     `GET /v1/models` with headers `x-api-key` and `anthropic-version: 2023-06-01`.
     Count only real IDs matching
     `^claude-(opus|sonnet|haiku|fable|mythos)-[0-9]+(-[0-9]+)?(-[0-9]{8})?$`.
     The `claude-fable-5-dd-*` entries are Claude Code destination aliases, not
     models.
   - Routing gets new models from the official remote catalog
     (`router-for-me/models`, refreshed every 3 h). A deploy cannot add them.
2. **Prove each new model** with one tiny real request.
   - Codex: `POST /v1/responses` with `"reasoning":{"effort":"low"}`,
     `"store":false` and the input "Reply with exactly: ok".
   - Claude: `POST /v1/messages` with `max_tokens` 16 and headers `x-api-key` and
     `anthropic-version: 2023-06-01`.
   - Record the status. A non-quota failure starts a class M search in the new
     upstream commits.
3. **Codex picker catalog.**
   - Production serves `models.codex-catalog: /CLIProxyAPI/static/catalogs/codex-client-compat.json`.
     The user crontab block `cliproxy-catalog-compat` refreshes it hourly with
     `/DATA/AppData/CLIProxyAPI/.tmp/retained/cliproxy-catalog/refresh_catalog.py`.
   - If the file is older than the official `codex_client_models.json`, or lacks
     a routable `gpt-*` model, check the crontab path first. Repair only that
     marked block, after backing up `crontab -l` into the helper's `backups/`.
     Then run the block's command once; it validates and publishes atomically.
   - The router loads the file on its 3 h ticker, or at once on any restart.
   - A routable model missing from the file is shown to Codex with approximate
     fallback-template metadata. That is a reason to refresh, not to deploy.
4. **Codex client version.**
   - The local Codex lives in the codex-config runtime
     `~/.local/share/codex-config/runtime/node-v22.23.3-linux-x64`.
   - If a new model's official `minimal_client_version` is above
     `<runtime>/bin/codex --version`, upgrade it:
     `PATH=<runtime>/bin:$PATH npm install -g --prefix <runtime> @openai/codex@<v>`.
     The prefix is required, because the default npm prefix is `/usr/local`.
   - `<v>` is the newest npm release (`npm view @openai/codex time`) that was
     published at least 24 h earlier and meets the minimum. Never downgrade.
   - Then smoke-test from `.tmp/codex-smoke` with
     `codex exec --ephemeral --skip-git-repo-check --sandbox read-only -m <model> 'Reply with exactly: ok'`.
   - Running sessions keep their old binary until restarted; report that.
5. **Advisor switch:** see below. It needs a restart, so it uses step 7.

## Advisor switch

The subagent advisor (`subagent-advisor.models` in `config.yaml`) is switched to
a newer model of the same family when all of these hold:

- **Same line.** The candidate is in the same line as the current entry:
  - Codex Sol: `gpt-<major>[.<minor>]-sol`;
  - Claude Opus: `claude-opus-<major>-<minor>`.

  Never switch to Fable, Mythos, Haiku, Astra, Terra or Luna. The DeepSeek entry
  is not switched automatically.
- **Newer.** It is a strictly newer version.
- **Ready.** It is routable, its test request returned 200, and it is in the
  official routing catalog.
- **Picker-ready (Codex).** The local Codex meets its `minimal_client_version`.

How to stage the switch:

1. Copy the live `config.yaml`, mode 0600, to a private
   `.tmp/deploy-advisor-<model>-YYYYMMDD/` directory, keeping a `config.before`
   copy.
2. Change only the `subagent-advisor.models` list. Keep the full list, because
   the list replaces the built-in one.
3. In the switched entry, set `name` to the new ID and `match` to
   `[<new>, <previous>]`. Copy everything else (efforts, modes, `price`,
   `max-output-tokens`, harnesses) unchanged, and note that the price is
   inherited until real pricing is known.

**Config check.**
- In a detached throwaway worktree at the served commit, run a temporary test
  that calls `internal/config.ParseConfigBytes` on both files.
- It must show three things:
  - the staged file passes `SubagentAdvisor.Validate`;
  - the resolved models differ only in that entry's `name` and `match`;
  - everything else, config and resolved advisor alike, is identical.
- Remove the worktree afterwards.

**Claude Code clients** start Opus through the `opus` alias, which they resolve
locally (`ANTHROPIC_DEFAULT_OPUS_MODEL`). The run does not edit client or fleet
configuration. It reports that the operator must update those defaults.

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
   - Report the outcome of any deploy the previous run left unknown (`state.json`
     `deploys`, `status.json`).
1. **Collect.** Run `git fetch upstream --tags` and `git fetch origin`.
   - List the commits in `last_reviewed_upstream..upstream/main`, plus releases.
   - List advisories:
     `gh api repos/router-for-me/CLIProxyAPI/security-advisories --jq '.[]|[.ghsa_id,.severity,.published_at,.summary]|@tsv'`.
   - Run the model check above.
   - If there are no new commits, no new advisories and no model findings, post
     a one-line report and stop.
2. **Triage.** Production profile: Claude OAuth, Codex OAuth and the OpenCode Go
   plugin provider. Clients are Claude Code on `/v1/messages` and Codex/pi on
   `/v1/responses` (gpt-* models).
   - Gemini, Antigravity, Devin, Vertex, Home, SIWC and Muse are unused.
   - Drop anything already in the fork. Search `git log --all --grep <sha>` and
     `docs/integration/`.
   - Check each candidate against `logs/main.log` (root-owned; `sudo -n`; covers
     about 11 to 30 h) and against the request logs (about 1 h). Grep with `-F`/`-l` under
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
   - the upstream commit(s) and the ported diff, or the advisor config diff;
   - the class, the evidence, and the protected-path rule;
   - an instruction to check that routing, cooldowns, OAuth identities,
     conversation ownership, leases, redemption, journals, pending UUIDs, config
     and defaults are unchanged.

   Fix what it finds, re-run the gates, and start a fresh review. After three
   rounds without approval, report a blocker and do not deploy.
6. **Deploy A or B (idle cutover).**
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
     Run it from `.worktrees/usage-split-20261111`, as a `systemd-run --wait` unit.
     It must pass, including the compose dry run. Do not use an outage budget
     above 40.
   - **Launch:** `sudo -n python3 ops/idle-deploy/main.py launch --now --release .../release.json`.
     `--now` skips only the five-minute quiet period. It still requires zero
     active requests and zero non-dormant connections, so it never cuts an active
     request. Never run `docker compose up/restart/stop/rm` against production.
   - **Fallback:** after multi-day uptime, the first start imports rotated usage
     logs and an idle prepare often fails the 40 s budget. A class A/B fix may
     then ride in a step 7 release of the same run. Otherwise report it for the
     operator.
7. **Deploy M or advisor switch (interrupting cutover).** The operator authorized
   this mode on 2026-11-18 for these changes. It cuts in-flight requests, usually
   for 10 to 20 s.
   - **Contents:** one release may carry:
     - the class M port;
     - the advisor switch (`--config`);
     - class A/B fixes from the same run that passed their gates and review;
     - `approved_to_ride` commits, when an image is built anyway.

     Never ship anything in `pending_unapproved`. A config-only release reuses the
     served image and commit.
   - **Single flight** as in step 6. At most one interrupting launch per run.
   - **Prepare:** `sudo -n python3 ops/idle-deploy/main.py prepare --image ... --commit <full sha> --retire-idle-clients --outage-budget 70 [--config <staged>]`.
     Run it as a `systemd-run --wait` unit from `.worktrees/usage-split-20261111`.
     It must pass, including the compose dry run and both fixture cycles.
     Prepare may fail on a transient fixture timing. Retry at most twice, and
     only after reading the cause in `.tmp/idle-deploy/private-error.log`.
   - **Multi-family review (required before launch).** Review the exact prepared
     release with fresh read-only delegates from different model families. Start
     a new `delegate_task` for each, in parallel:
     - GPT: Codex provider, `gpt-6.2-sol` (or the newest Sol), effort high;
     - Claude: Claude provider, `claude-opus-5-5` (newest Opus; never Fable),
       fresh session;
     - DeepSeek: OpenCode provider, `opencode/deepseek-v4.1-flash-free`, when
       available.

     **Brief.** Give every reviewer the same brief:
     - `release.json` pins (image, commit, budget, retirement) and the rehearsal
       timings and fixture cycle results;
     - the ported diff and gate results, or the advisor `models` diff and config
       check output;
     - `state.json` `pending_unapproved` and `approved_to_ride`, plus the
       protected-path list;
     - the expected break.

     **Never include** `config.yaml` beyond the advisor block, `.env`, keys or
     credentials.

     Each reviewer answers `APPROVE` or `REJECT` with reasons. It must check that:
     - nothing beyond the stated change ships;
     - routing, cooldowns, OAuth identities, conversation ownership, leases,
       redemption, journals, pending UUIDs, config and defaults are unchanged;
     - the timing margins are sane.

     **Decision.** Launch only when:
     - the GPT and Claude reviewers both approve;
     - no reviewer rejects (a DeepSeek rejection also blocks);
     - an unavailable DeepSeek reviewer is recorded as such.

     The orchestrator never overrules a rejection. Fix what was found and run
     fresh reviews. After three rounds without unanimous approval, leave the
     release prepared and report it.
   - **Launch window:** only between 04:00 and 07:00 Europe/Berlin. Run
     `sudo -n python3 ops/idle-deploy/interrupt.py launch .../release.json`.
     Outside the window, leave the release prepared (it expires after 24 h) and
     report it. Never run `docker compose up/restart/stop/rm` against production.
   - Record the pending deploy in `state.json` before launching. This agent loses
     its model connection during the cutover.
8. **Recovery and outcome (steps 6 and 7).**
   - The runner is a detached systemd unit. It starts the router, then requires
     State `running`, `/healthz` 200 and an authenticated `/v1/models` 200 within
     about 60 s. Otherwise it restores the previous image, `.env` pin and
     `config.yaml`, verifies them the same way, and keeps retrying through an
     independent rescue unit until something serves.
   - Read the outcome only from
     `output/verification/idle-deploy/RELEASE/status.json` (`deployed`,
     `rolled_back`, `original_retained`, ...). Never report success without
     `deployed` and its host verification.
   - Also report `interruption_seconds` and the interrupted request and
     connection counts from `.tmp/idle-deploy/RELEASE/transaction.json`, plus
     `live-verification.json` and `codex-verdict.json`.
   - After a class M or advisor deploy, repeat the new model's test request, and
     for an advisor switch check `GET /v1/account-policy/subagent-advice?format=json`.
   - If the run ends before the outcome is known, the next run reports it.
9. **Record.**
   - Write `docs/integration/upstream-review-YYYYMMDD.md`: decisions and evidence
     per commit and per model, and the gates and review verdicts (every family).
   - Update `state.json`. Set `last_reviewed_upstream` only after triage is
     complete. Update `known_models`, `advisor_models` and `codex_client_version`.
     Add a deploy record when a deploy happens.
   - After a deploy, update the production memory note
     (`production-branch-and-deploy`). Do not push unless the operator asks.
10. **Report.** End with a short report:
    - what is new upstream and what was taken, and why;
    - new models, what the model check fixed (catalog, Codex upgrade, advisor),
      and each test request's status;
    - what was deployed, with its `status.json` outcome and interruption;
    - what waits for the operator, with a recommendation (including Claude client
      defaults after an Opus switch);
    - watch items.
