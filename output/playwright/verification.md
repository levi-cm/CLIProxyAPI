# Playwright verification index

Verified with system Chromium through the Playwright CLI on2026-10-04 against
the fixture-only private preview at100.82.251.30:18318.

The full checked flows and limits are recorded in
[account-policy-verification.md](../../docs/account-policy-verification.md).

Local ignored artifacts:

- `final-desktop-policy.png`, `final-desktop-viewport.png`
- `desktop-policy.png`, `mobile-policy.png`
- `diagnostics-fixture.json`
- `.playwright-cli/` DOM snapshots, console logs and downloaded fixture JSON

Countdown check:45s->42s, request counts51->51; expired control disabled after
fake-clock advancement. Mobile width390px/document390px. BerlinOctober29GMT+1
and fixedGMT+2 preserve the same instant. Selected reset consumed October5 only,
with remaining October22/29 untouched. No live reset operations occurred.

Dashboard follow-up: Playwright exercised a 100-account pool with ten pages,
concurrent serving accounts, unknown/stale/failure recovery, runtime-only
providers, draft/selection ownership and keyboard focus retention. Passive
fixture refresh/settings/reset write counters stayed zero. 390px and320px
documents had no horizontal overflow; charts use internal scroll regions.
Authenticated Lighthouse snapshot accessibility:100 across four views in both
themes. Local screenshots: `dashboard-final-desktop.png`,
`dashboard-final-mobile.png`. Repeatable scripts are in
`internal/accountpolicyui/scripts/`; full details in the verification document.
