/* Real-browser regressions. Run only against cmd/account-policy-preview. */
const assert = require("node:assert/strict");
const { chromium } = require(
  process.env.PLAYWRIGHT_MODULE_PATH || "playwright",
);
const base = process.env.ACCOUNT_POLICY_PREVIEW_URL || "http://127.0.0.1:18318";

(async () => {
  const browser = await chromium.launch({
    executablePath: process.env.CHROMIUM_PATH || "/usr/bin/chromium",
    headless: true,
    args: ["--no-sandbox"],
  });
  try {
    const page = await browser.newPage({
      viewport: { width: 1440, height: 1000 },
    });
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const auth = { Authorization: "Bearer fixture-key" };
    const state = async () => {
      const response = await page.request.get(base + "/fixture/state", {
        headers: auth,
      });
      assert.equal(
        response.ok(),
        true,
        "Must be a private fixture preview, never production",
      );
      const data = await response.json();
      assert.equal(
        data.fixture_only,
        true,
        "Refuse mutation without fixture-only evidence",
      );
      return data;
    };
    await state();
    const control = async (data) => {
      const response = await page.request.post(base + "/fixture/control", {
        headers: auth,
        data,
      });
      assert.equal(response.ok(), true);
    };
    await control({ reset: true });
    await control({
      account_count: 100,
      active_accounts: ["account-a", "account-100"],
    });
    await page.goto(base + "/account-policy.html");
    await page
      .getByRole("textbox", { name: "Management key" })
      .fill("fixture-key");
    await page.getByRole("button", { name: "Connect", exact: true }).click();
    await page.locator("#workspace").waitFor({ state: "visible" });
    assert.equal(await page.locator(".serving-account").count(), 2);
    assert.ok((await page.locator(".chart-bar").count()) > 0);
    assert.equal(
      await page.locator("#allowances .allowance-account").count(),
      10,
    );
    assert.ok((await page.locator(".weekly-cycle-remaining").count()) > 0);
    assert.ok((await page.locator(".weekly-cycle-elapsed").count()) > 0);
    assert.equal(await page.locator(".timeline-marker.weekly").count(), 0);
    assert.ok((await page.locator(".timeline-marker.expiry line").count()) > 0);
    await page.getByRole("button", { name: "Accounts", exact: true }).click();
    assert.equal(await page.locator("#accounts article").count(), 10);
    assert.match(
      await page.locator("#page-summary").textContent(),
      /Page 1 of 10/,
    );
    await page.getByRole("button", { name: "Next", exact: true }).click();
    assert.match(
      await page.locator("#page-summary").textContent(),
      /Page 2 of 10/,
    );
    await page
      .getByRole("searchbox", { name: "Find account" })
      .fill("account-100");
    assert.equal(
      await page.locator("#accounts article").getAttribute("data-account"),
      "account-100",
    );
    await page.getByRole("searchbox", { name: "Find account" }).fill("");
    await page
      .getByRole("combobox", { name: "Status", exact: true })
      .selectOption("active");
    assert.equal(await page.locator("#accounts article").count(), 2);
    await page
      .getByRole("combobox", { name: "Status", exact: true })
      .selectOption("");
    await page
      .getByRole("searchbox", { name: "Find account" })
      .fill("account-a");
    const details = page.locator("[data-account=account-a] .account-details");
    await details.locator("summary").click();
    await details.locator(".reserve").fill("17");
    await details.locator("summary").click();
    await page.getByRole("button", { name: "Overview", exact: true }).click();
    const beforePoll = (await state()).dashboard_requests;
    await page.waitForResponse((response) =>
      response.url().includes("/dashboard?range="),
    );
    await page.waitForFunction(
      () =>
        document.querySelector("#accounts article > .field-help")
          ?.textContent === "Unsaved account changes",
    );
    await page.locator("#zone").selectOption("UTC");
    await page.getByRole("button", { name: "Accounts", exact: true }).click();
    await details.locator("summary").click();
    assert.equal(await details.locator(".reserve").inputValue(), "17");
    await page
      .getByRole("button", { name: "Routing & settings", exact: true })
      .click();
    await page.locator("#fallback").selectOption("fill-first");
    await page.getByRole("button", { name: "Overview", exact: true }).click();
    const serving = page.locator(".serving-account").first();
    await serving.focus();
    const servingID = await serving.getAttribute("data-open-account");
    await page.waitForResponse((response) =>
      response.url().includes("/dashboard?range="),
    );
    assert.equal(
      await page.evaluate(() => document.activeElement.dataset.openAccount),
      servingID,
    );
    // Account A's distant credits are intentionally absent from the main axis.
    const cycle = page.locator(".weekly-cycle").first();
    await cycle.focus();
    const cycleID = await cycle.getAttribute("data-focus-key");
    await page.waitForResponse((response) =>
      response.url().includes("/dashboard?range="),
    );
    assert.equal(
      await page.evaluate(() => document.activeElement.dataset.focusKey),
      cycleID,
    );
    await control({ stale_activity: true });
    await page.waitForFunction(
      () =>
        document.querySelector("#activity-badge").textContent ===
        "Stale activity",
      null,
      { timeout: 15000 },
    );
    assert.equal(await page.locator(".serving-account").count(), 0);
    await control({ stale_activity: false, dashboard_unavailable: true });
    await page.waitForFunction(
      () =>
        document.querySelector("#activity-badge").textContent ===
        "Activity unavailable",
      null,
      { timeout: 15000 },
    );
    await control({ dashboard_unavailable: false });
    await page.waitForFunction(
      () =>
        document.querySelector("#activity-badge").textContent ===
        "Live activity",
      null,
      { timeout: 15000 },
    );
    await page
      .getByRole("button", { name: "Routing & settings", exact: true })
      .click();
    assert.equal(await page.locator("#fallback").inputValue(), "fill-first");
    await page.getByRole("button", { name: "Overview", exact: true }).click();
    await page.route(
      "**/v8/management/account-policy/accounts",
      async (route) => {
        const response = await route.fetch();
        const json = await response.json();
        json.accounts = json.accounts.filter(
          (a) => a.identity.provider === "codex",
        );
        await route.fulfill({ response, json });
      },
    );
    await page
      .getByRole("button", { name: "Reload snapshots", exact: true })
      .click();
    await page.getByRole("button", { name: "Accounts", exact: true }).click();
    await page.getByRole("searchbox", { name: "Find account" }).fill("");
    await page
      .getByRole("combobox", { name: "Provider", exact: true })
      .selectOption("claude");
    assert.ok((await page.locator("#accounts article").count()) > 0);
    assert.match(
      await page.locator("#accounts article").first().textContent(),
      /Quota, plan and reset expiry are unavailable/,
    );
    await page
      .getByRole("combobox", { name: "Theme", exact: true })
      .selectOption("dark");
    for (const width of [390, 320]) {
      await page.setViewportSize({ width, height: 844 });
      assert.equal(
        await page.evaluate(() => document.documentElement.scrollWidth),
        width,
      );
      assert.equal(await page.locator("#theme").isVisible(), true);
    }
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.unrouteAll();
    // Exercise schedule drafts without enabling or writing production policy.
    await page.route(
      "**/v8/management/account-policy/accounts",
      async (route) => {
        const response = await route.fetch();
        const json = await response.json();
        json.settings.enabled = true;
        for (const account of json.accounts)
          if (account.identity.credential_id === "account-b") {
            account.observed_at = account.inventory_observed_at =
              new Date().toISOString();
            // Draft tests must not depend on a fixed fixture expiry being future.
            for (const credit of account.credits)
              if (credit.expires_at != null)
                credit.expires_at = new Date(
                  Date.now() + 3600000,
                ).toISOString();
          }
        await route.fulfill({ response, json });
      },
    );
    await page
      .getByRole("button", { name: "Reload snapshots", exact: true })
      .click();
    await page
      .getByRole("combobox", { name: "Provider", exact: true })
      .selectOption("");
    await page
      .getByRole("searchbox", { name: "Find account" })
      .fill("account-b");
    const owner = page.locator("[data-account=account-b]");
    await owner.locator(".account-details > summary").click();
    await owner
      .locator('[data-action="schedule"][data-credit="october-05"]')
      .click();
    await owner.locator(".schedule-at").fill("2026-10-05T03:50:00Z");
    await owner
      .locator('[data-action="schedule"][data-credit="october-22"]')
      .click();
    await page.locator("#zone").selectOption("Europe/Berlin");
    assert.equal(
      await owner.locator(".schedule-form").getAttribute("data-credit"),
      "october-22",
    );
    assert.equal(
      await owner.locator(".schedule-at").inputValue(),
      "2026-10-05T03:50:00Z",
    );
    const final = await state();
    assert.ok(final.dashboard_requests > beforePoll);
    assert.equal(final.provider_refreshes, 0);
    assert.equal(final.settings_writes, 0);
    assert.equal(final.writes, 0);
    assert.deepEqual(
      await page.evaluate(() => [localStorage.length, sessionStorage.length]),
      [0, 0],
    );
    await page.getByRole("button", { name: "Disconnect", exact: true }).click();
    assert.equal(await page.locator("#workspace").isVisible(), false);
    assert.equal(await page.locator("#accounts article").count(), 0);
    assert.equal(
      await page.locator("#allowances .allowance-account").count(),
      0,
    );
    assert.deepEqual(errors, []);
    console.log(
      "PASS: 100-account pool, concurrent serving, graphs, drafts, keyboard focus, stale/failure recovery, runtime-only providers, mobile, no provider writes, memory-only key",
    );
  } finally {
    await browser.close();
  }
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
