# Authentication snapshot follow-up

Base: validated repair `e8b4cab67b0f8dd6cd379a9be01ebe4205ff3dd2`.

## Selected upstream fixes

- [215535096 (#6948)](https://github.com/router-for-me/CLIProxyAPI/commit/21553509662eba4b8e889d797822c1891c980b5d): recover routing after successful login replaces credentials that failed refresh, while retaining active quota cooldowns.
- [32dd2c86 (#6951)](https://github.com/router-for-me/CLIProxyAPI/commit/32dd2c86b557b4a02dbe8b19baeab39b4ab0f8b6): retain the persisted file snapshot binding so a delayed login update or reload cannot reverse an operator disable.

Both are worth integrating: their regression tests failed against the deployed
repair before implementation, then passed after applying the upstream changes.
The patches applied cleanly. Required snapshot prerequisites `c4c1d43e` and
`d5816d5e` were already ancestors of the deployed source. They were not reapplied,
and no broad upstream upgrade or dependency update is bundled.

The reported `17199e47` monitoring baseline describes GitHub `origin/main`, not
the actual serving source. The original serving source was `b8a73830`, followed
by the locally built repair `e8b4cab6`. The merge base of this local repair and
the selected upstream fixes is `8289ec4c`, which already contains the prerequisites.

Custom account policy, earliest-renewal routing, mandatory automatic refills,
conversation ownership and credential identity checks are retained. In particular,
the existing custom SIWC active-quota preservation guard remains intact. No
production credentials or policy state were used as test writers.

## Diagnostic formatter correction

Live verification of the first repair found that the global formatter suppressed
its sanitized structured fields. This follow-up emits an explicit field allowlist
only for the `account-policy-rejection` and `codex-capacity` components. String
values are quoted to keep each entry on one line. Prompt, encrypted content,
token and raw-session fields remain excluded. A regression exercises the actual
formatter, including extra protected fields and newline injection, rather than
only inspecting a log hook.

## Validation

Both upstream reproduction groups failed before the patch and passed after it.
The formatter regression also failed before its correction and passed afterward.
A fresh read-only review found no issues in this follow-up diff. Race tests
passed for nine packages covering auth, watcher, service, account policy, bindings,
logging and API. The required server compilation passed through
`scripts/go-limited.sh` with Go 1.26.10. The complete Go suite passed, including
111 packages with tests. Live deployment evidence is recorded separately in the
protected deployment report after completion.

Production replacement requires the same explicit drained-maintenance budget,
writer exclusion, preserved durable state and tested rollback as the first repair.
