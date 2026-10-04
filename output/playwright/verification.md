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
