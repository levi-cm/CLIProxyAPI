/* Observation-only display regressions. Fixture-only; no provider or settings writes. */
const assert = require("node:assert/strict");
const { chromium } = require(
  process.env.PLAYWRIGHT_MODULE_PATH || "playwright",
);
const base = process.env.ACCOUNT_POLICY_PREVIEW_URL || "http://127.0.0.1:18318";

(async () => {
  const browser = await chromium.launch({
    executablePath: "/usr/bin/chromium",
    headless: true,
    args: ["--no-sandbox"],
  });
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const auth = { Authorization: "Bearer fixture-key" };
    const readState = async () => {
      const response = await page.request.get(base + "/fixture/state", {
        headers: auth,
      });
      assert.equal(response.ok(), true);
      const data = await response.json();
      assert.equal(data.fixture_only, true, "Refuse non-fixture target");
      return data;
    };
    const before = await readState();
    let recording = true;
    let healthy = true;
    await page.route(
      "**/v8/management/account-policy/accounts",
      async (route) => {
        const response = await route.fetch();
        const json = await response.json();
        json.settings.enabled = false;
        json.settings.observations_enabled = recording;
        json.settings.automation = "off";
        json.accounts = json.accounts.slice(0, 2);
        for (const account of json.accounts) {
          account.observed_at = account.inventory_observed_at = new Date(
            Date.now() - 300000,
          ).toISOString();
          account.available_credits = 2;
          for (const bucket of account.buckets)
            bucket.observed_at = account.observed_at;
        }
        await route.fulfill({ response, json });
      },
    );
    await page.route(
      "**/v8/management/account-policy/dashboard?**",
      async (route) => {
        const response = await route.fetch();
        const json = await response.json();
        json.usage = {
          available: recording && healthy,
          collecting: recording,
          totals: {
            requests: 0,
            success: 0,
            failed: 0,
            total_tokens: null,
            average_latency_ms: null,
          },
          series: [],
          accounts: [],
        };
        await route.fulfill({ response, json });
      },
    );
    await page.goto(base + "/account-policy.html");
    await page
      .getByRole("textbox", { name: "Management key" })
      .fill("fixture-key");
    await page.getByRole("button", { name: "Connect", exact: true }).click();
    await page.locator("#workspace").waitFor({ state: "visible" });
    const metric = (label) =>
      page
        .locator(".metric")
        .filter({ has: page.locator(".metric-label", { hasText: label }) })
        .locator(".metric-value");
    assert.equal(await metric("Recorded attempts").textContent(), "0");
    assert.equal(await metric("Success rate").textContent(), "No attempts yet");
    assert.equal(
      await metric("Measured tokens").textContent(),
      "Waiting for completions",
    );
    assert.equal(
      await metric("Average latency").textContent(),
      "Waiting for completions",
    );
    assert.equal(
      await metric("Last known weekly allowance").textContent(),
      "33%",
    );
    assert.equal(await metric("Last known saved resets").textContent(), "4");
    assert.match(
      await page.locator("#coverage").textContent(),
      /Collection is running/,
    );
    await page
      .getByRole("button", { name: "Routing & settings", exact: true })
      .click();
    assert.equal(await page.locator("#observations-enabled").isChecked(), true);
    assert.equal(await page.locator("#enabled").isChecked(), false);
    assert.equal(await page.locator("#automation").inputValue(), "off");
    assert.match(
      await page.locator("#policy-status").textContent(),
      /Dashboard collection only/,
    );
    await page.getByRole("button", { name: "Accounts", exact: true }).click();
    assert.match(
      await page
        .locator("[data-account=account-a] .account-quota-summary")
        .textContent(),
      /Last known weekly allowance 33%/,
    );
    assert.equal(await page.locator("[data-action=redeem]:enabled").count(), 0);
    healthy = false;
    await page
      .getByRole("button", { name: "Reload snapshots", exact: true })
      .click();
    await page.getByRole("button", { name: "Overview", exact: true }).click();
    assert.equal(await metric("Measured tokens").textContent(), "Unavailable");
    assert.match(
      await page.locator("#coverage").textContent(),
      /storage errors/,
    );
    healthy = true;
    recording = false;
    await page
      .getByRole("button", { name: "Reload snapshots", exact: true })
      .click();
    await page.getByRole("button", { name: "Overview", exact: true }).click();
    assert.equal(
      await metric("Recorded attempts").textContent(),
      "Collection off",
    );
    assert.match(
      await page.locator("#coverage").textContent(),
      /Enable Collect dashboard data/,
    );
    const after = await readState();
    for (const key of ["provider_refreshes", "settings_writes", "writes"])
      assert.equal(after[key], before[key]);
    assert.deepEqual(errors, []);
    console.log(
      "PASS: observation-only settings, honest empty usage, retained stale quota/counts, no reset authority or writes",
    );
  } finally {
    await browser.close();
  }
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
