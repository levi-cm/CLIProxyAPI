/* Exercise mandatory-redemption controls against private fixture data only. */
const assert = require("node:assert/strict");
const { chromium } = require(process.env.PLAYWRIGHT_MODULE_PATH || "playwright");
const base = process.env.ACCOUNT_POLICY_PREVIEW_URL || "http://127.0.0.1:18342";
(async () => {
  const browser = await chromium.launch({ executablePath: "/usr/bin/chromium", headless: true, args: ["--no-sandbox"] });
  try {
    for (const width of [1440, 390]) {
      const page = await browser.newPage({ viewport: { width, height: 1000 } });
      try {
        const response = await page.request.get(base + "/fixture/state", { headers: { Authorization: "Bearer fixture-key" } });
        assert.equal(response.ok(), true, "Requires private fixture preview");
        assert.equal((await response.json()).fixture_only, true);
        let locked = false, writes = 0;
        const errors = [];
        page.on("pageerror", (error) => errors.push(error.message));
        await page.route("**/v8/management/account-policy/**", async (route) => {
          if (route.request().method() === "PATCH") {
            writes++;
            return route.fulfill({ status: 500, contentType: "application/json", body: '{"error":{"message":"Unexpected settings write"}}' });
          }
          const original = await route.fetch();
          const json = await original.json();
          if (json.settings && locked) Object.assign(json.settings, { automation_required: true, enabled: true, automation: "auto_expiring", read_only: false });
          return route.fulfill({ response: original, json });
        });
        await page.goto(base + "/account-policy.html");
        await page.locator("#key").fill("fixture-key");
        await page.locator("#login-form button").click();
        await page.locator("#workspace").waitFor({ state: "visible" });
        await page.locator('[data-view="routing"]').click();
        await page.getByText("Advanced routing & reset safety", { exact: true }).click();
        await page.locator("#fallback").selectOption("fill-first");
        await page.locator("#guard").fill("123");
        locked = true;
        await page.locator("#reload").click();
        await page.waitForFunction(() => document.querySelector("#notice").textContent.includes("Snapshots reloaded"));
        assert.equal(await page.locator("#enabled").isDisabled(), true, "A background lock must override stale form drafts");
        assert.equal(await page.locator("#enabled").isChecked(), true);
        assert.equal(await page.locator("#read-only").isDisabled(), true);
        assert.equal(await page.locator('#automation option[value="off"]').evaluate((option) => option.disabled), true);
        assert.equal(await page.locator('#automation option[value="notify"]').evaluate((option) => option.disabled), true);
        assert.equal(await page.locator("#automation").inputValue(), "auto_expiring");
        assert.equal(await page.locator("#guard").inputValue(), "600");
        assert.equal(await page.locator("#guard").getAttribute("min"), "600");
        assert.equal(await page.locator("#fallback").inputValue(), "fill-first", "Unrelated drafts should survive");
        assert.equal(await page.locator("#automation-lock-notice").isVisible(), true);
        await page.evaluate(() => {
          document.querySelector("#enabled").checked = false;
          document.querySelector("#settings-form").dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
        });
        await page.waitForFunction(() => document.querySelector("#notice").textContent.includes("cannot be disabled"));
        assert.equal(writes, 0, "Invalid draft should not issue a settings mutation");
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 2), false);
        assert.deepEqual(errors, []);
      } finally {
        await page.unrouteAll({ behavior: "wait" });
        await page.close();
      }
    }
    console.log("PASS: desktop/mobile mandatory-redemption controls, stale drafts, minimum expiry guard, local rejection, no writes");
  } finally { await browser.close(); }
})().catch((error) => { console.error(error); process.exitCode = 1; });
