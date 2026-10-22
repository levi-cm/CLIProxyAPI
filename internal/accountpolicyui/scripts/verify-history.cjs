/* Fixture browser regression: intercept only the newly introduced history APIs. */
const assert = require("node:assert/strict");
const { chromium } = require(
  process.env.PLAYWRIGHT_MODULE_PATH || "playwright",
);
(async () => {
  const browser = await chromium.launch({
    executablePath: "/usr/bin/chromium",
    headless: true,
    args: ["--no-sandbox"],
  });
  const errors = [];
  let override = { schema_version: 1, revision: 0, state: "inactive" },
    mutations = 0;
  try {
    for (const viewport of [
      { width: 1440, height: 1000 },
      { width: 390, height: 844 },
    ]) {
      const page = await browser.newPage({ viewport });
      page.on("pageerror", (e) => errors.push(e.message));
      const base = process.env.ACCOUNT_POLICY_PREVIEW_URL || "http://127.0.0.1:18328";
      const headers = { Authorization: "Bearer fixture-key" };
      const fixture = await page.request.get(base + "/fixture/state", { headers });
      assert.equal(fixture.ok(), true, "Requires a private fixture preview");
      assert.equal((await fixture.json()).fixture_only, true, "Refuse non-fixture mutation");
      const expanded = await page.request.post(base + "/fixture/control", {
        headers,
        data: { account_count: 100 },
      });
      assert.equal(expanded.ok(), true);
      await page.route("**/v8/management/account-policy/**", async (route) => {
        const request = route.request(),
          url = new URL(request.url()),
          path = url.pathname.split("/account-policy")[1];
        const json = (value) =>
          route.fulfill({
            status: 200,
            contentType: "application/json",
            body: JSON.stringify(value),
          });
        if (path === "/routing-override") {
          if (request.method() === "PUT") {
            const body = request.postDataJSON();
            assert.equal(body.expected_revision, override.revision);
            mutations++;
            override = {
              schema_version: 1,
              id: "fixture-override",
              revision: override.revision + 1,
              state: "active",
              mode: body.mode || "single",
              accounts: body.accounts,
              credential_id: body.credential_id,
              started_at: new Date().toISOString(),
              expires_at: new Date(
                Date.now() + body.duration_hours * 3600000,
              ).toISOString(),
            };
          }
          if (request.method() === "DELETE") {
            mutations++;
            override = {
              ...override,
              revision: override.revision + 1,
              state: "cancelled",
              termination_reason: "cancelled",
            };
          }
          return json(override);
        }
        if (path === "/allowance-history") {
          const end = Date.parse(url.searchParams.get("to")),
            from = Date.parse(url.searchParams.get("from"));
          return json({
            schema_version: 1,
            status: { enabled: true, healthy: true },
            observations: [
              {
                credential_id: url.searchParams.get("credential_id"),
                scope: "ordinary",
                observed_at: new Date(
                  Math.min(Date.now() - 3600000, end - 3600000),
                ).toISOString(),
                remaining_percent: 65,
                event: "observation",
              },
              {
                credential_id: url.searchParams.get("credential_id"),
                scope: "ordinary",
                observed_at: new Date(
                  Math.max(from, Math.min(Date.now() - 3540000, end - 3540000)),
                ).toISOString(),
                remaining_percent: 95,
                event: "manual_reset",
              },
            ],
          });
        }
        if (path === "/usage-history/status")
          return json({
            schema_version: 1,
            enabled: true,
            healthy: true,
            write_errors: 0,
          });
        if (path === "/usage-history/requests")
          return json({
            schema_version: 1,
            records: [
              {
                requested_at: new Date().toISOString(),
                requested_model: "alias-fixture",
                execution_model: "execution-fixture",
                response_model: "response-fixture",
                credential_id: "account-a",
                outcome: "success",
                tokens: {
                  quality: "complete",
                  input: { total_tokens: 1000 },
                  output: { total_tokens: 20 },
                },
                cached_input_percent: 8,
              },
            ],
          });
        if (path === "/usage-history/summary")
          return json({
            schema_version: 1,
            groups: [
              {
                period: "2026",
                provider: "codex",
                model: "execution-fixture",
                credential_id: "account-a",
                provider_attempts: 1,
                input_tokens: 1000,
                output_tokens: 20,
                cached_input_percent: 8,
                cache_measured_attempts: 1,
              },
            ],
          });
        return route.continue();
      });
      await page.goto(base + "/account-policy.html");
      await page.locator("#key").fill("fixture-key");
      await page.locator("#login-form button").click();
      await page.locator("#workspace").waitFor({ state: "visible" });
      await page.waitForFunction(
        () => document.querySelectorAll(".allowance-day").length === 7,
      );
      await page.locator('[data-view="routing"]').click();
      assert.equal(await page.locator("#override-hours option").count(), 24);
      await page.locator("#override-account").selectOption("account-b");
      await page.locator("#override-hours").selectOption("24");
      await page.locator("#override-apply").click();
      await page.waitForFunction(() =>
        document
          .querySelector("#override-status")
          .textContent.includes("expires"),
      );
      await page.locator("#reload").click();
      await page.waitForTimeout(150);
      assert.equal(await page.locator("#override-hours").inputValue(), "24");
      assert.equal(
        await page.locator("#override-account").inputValue(),
        "account-b",
      );
      await page.locator("#override-cancel").click();
      await page.waitForFunction(() =>
        document
          .querySelector("#override-status")
          .textContent.includes("cancelled"),
      );
      await page.locator("#override-mode").selectOption("split");
      await page.locator("#override-account-secondary").selectOption("account-a");
      for (const percent of [50, 1, 99]) {
        await page.locator("#override-percent").fill(String(percent));
        assert.equal(await page.locator("#override-percent-other").textContent(), `${100 - percent}%`);
        await page.locator("#override-apply").click();
        await page.waitForFunction((percent) => {
          const text = document.querySelector("#override-status").textContent;
          return text.includes(`Account B ${percent}%`) && text.includes(`Account A ${100 - percent}%`) && text.includes("expires");
        }, percent);
        await page.locator("#reload").click();
        assert.equal(await page.locator("#override-percent").inputValue(), String(percent));
        if (percent === 50) {
          await page.screenshot({
            path: `/tmp/cliproxy-percentage-${viewport.width}.png`,
            fullPage: true,
          });
        }
        const overflow = await page.evaluate(() => [...document.querySelectorAll("body *")]
          .filter((element) => element.getBoundingClientRect().right > innerWidth + 2)
          .map((element) => `${element.tagName}#${element.id}.${element.className}`));
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 2), false, overflow.join(", "));
        await page.locator("#override-cancel").click();
        await page.waitForFunction(() => document.querySelector("#override-status").textContent.includes("cancelled"));
      }
      await page.locator('[data-view="history"]').click();
      await page
        .locator("#request-history")
        .getByText("execution-fixture", { exact: true })
        .waitFor();
      assert.match(
        await page.locator("#request-history").textContent(),
        /8.00%/,
      );
      assert.match(
        await page.locator("#request-history").textContent(),
        /response-fixture/,
      );
      assert.equal(
        await page.evaluate(
          () => document.documentElement.scrollWidth > innerWidth + 2,
        ),
        false,
      );
      await page.screenshot({
        path: "/tmp/cliproxy-history-" + viewport.width + ".png",
        fullPage: true,
      });
      await page.close();
    }
    assert.deepEqual(errors, []);
    assert.equal(mutations, 16);
    console.log(
      "PASS: desktop/mobile, daily allowance rows, 24 durations, single/split apply/cancel, 50/50 and 1/99 and 99/1, preserved drafts, model identities, cache percentage, no overflow",
    );
  } finally {
    await browser.close();
  }
})().catch((e) => {
  console.error(e);
  process.exitCode = 1;
});
