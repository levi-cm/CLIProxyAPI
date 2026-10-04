/* Real rendering checks: isolated fixture only, no provider or settings writes. */
const assert = require("node:assert/strict");
const { chromium } = require(
  process.env.PLAYWRIGHT_MODULE_PATH || "playwright",
);
const base = process.env.ACCOUNT_POLICY_PREVIEW_URL || "http://127.0.0.1:18319";
(async () => {
  const browser = await chromium.launch({
    executablePath: "/usr/bin/chromium",
    headless: true,
    args: ["--no-sandbox"],
  });
  try {
    const page = await browser.newPage({
      viewport: { width: 1440, height: 1000 },
    });
    const readState = async () => {
      const response = await page.request.get(base + "/fixture/state", {
        headers: { Authorization: "Bearer fixture-key" },
      });
      assert.equal(response.ok(), true);
      const data = await response.json();
      assert.equal(data.fixture_only, true);
      return data;
    };
    const before = await readState();
    const errors = [];
    page.on("pageerror", (e) => errors.push(e.message));
    await page.goto(base + "/account-policy.html");
    await page
      .getByRole("textbox", { name: "Management key" })
      .fill("fixture-key");
    await page.getByRole("button", { name: "Connect", exact: true }).click();
    await page.locator("#workspace").waitFor({ state: "visible" });
    assert.equal(await page.locator("#timeline-range").inputValue(), "7");
    assert.equal(
      await page.locator('#timeline-range option[value="30"]').count(),
      0,
    );
    const row = page.locator(".timeline-row").filter({
      has: page.locator(".timeline-name", { hasText: "Account A" }),
    });
    const full = Number(
      await row.locator(".weekly-cycle-outline").getAttribute("width"),
    );
    const elapsed = Number(
      await row.locator(".weekly-cycle-elapsed").getAttribute("width"),
    );
    assert.ok(
      Math.abs(elapsed / full - 3 / 7) < 0.002,
      "Elapsed cycle time must not be 67% quota used",
    );
    const filled = Number(
      await row.locator(".weekly-cycle-quota").getAttribute("height"),
    );
    const capacity = Number(
      await row.locator(".weekly-cycle-outline").getAttribute("height"),
    );
    assert.ok(
      Math.abs(filled / capacity - 0.33) < 0.0001,
      "Fill height must match 33% allowance left",
    );
    const later = page.locator(".timeline-row").filter({
      has: page.locator(".timeline-name", { hasText: "Account B" }),
    });
    assert.equal(await later.locator(".timeline-marker.expiry").count(), 1);
    assert.match(
      await later.locator(".timeline-later").textContent(),
      /2 later expiries/,
    );
    assert.doesNotMatch(
      await page.locator("#timeline").textContent(),
      /22\/10|29\/10/,
    );
    assert.ok((await page.locator(".timeline-marker.expiry line").count()) > 0);
    assert.equal(await page.locator(".timeline-marker.weekly").count(), 0);
    const overlaps = async () =>
      page.locator(".timeline-tick").evaluateAll((texts) => {
        const boxes = texts.map((t) => t.getBoundingClientRect());
        let count = 0;
        for (let a = 0; a < boxes.length; a++)
          for (let b = a + 1; b < boxes.length; b++)
            if (
              boxes[a].left < boxes[b].right &&
              boxes[b].left < boxes[a].right &&
              boxes[a].top < boxes[b].bottom &&
              boxes[b].top < boxes[a].bottom
            )
              count++;
        return count;
      });
    let axisClear = true;
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: 1000 });
      assert.equal(
        await page
          .locator("#timeline")
          .evaluate((el) => el.scrollWidth > el.clientWidth),
        false,
        "Timeline must fit, not hide bars in a horizontal scroller",
      );
      for (const range of ["1", "7"]) {
        await page.locator("#timeline-range").selectOption(range);
        axisClear &&= (await overlaps()) === 0;
        const nowPosition = await page
          .locator(".timeline-tick.now")
          .evaluate((label) => {
            const tick = label.getBoundingClientRect(),
              axis = label.parentElement.getBoundingClientRect();
            return (tick.left + tick.width / 2 - axis.left) / axis.width;
          });
        assert.ok(
          Math.abs(nowPosition - (range === "1" ? 0.858333333 : 0.5)) < 0.001,
          "Now label must align with the SVG line under the real CSP",
        );
      }
      assert.equal(
        await page.evaluate(() => document.documentElement.scrollWidth),
        width,
      );
    }
    await page.setViewportSize({ width: 1440, height: 1000 });
    let hoverReadable = true;
    for (const theme of ["light", "dark"]) {
      await page.locator("#theme").selectOption(theme);
      const geometry = await row.evaluate((r) => {
        const fill = r.querySelector(".weekly-cycle-quota");
        const base = r.querySelector(".weekly-cycle-outline");
        const colour = getComputedStyle(fill);
        return {
          ratio:
            fill.getBoundingClientRect().height /
            base.getBoundingClientRect().height,
          opacity: colour.opacity,
          fill: colour.fill,
        };
      });
      assert.ok(Math.abs(geometry.ratio - 0.33) < 0.0001);
      assert.equal(geometry.opacity, "1");
      assert.doesNotMatch(geometry.fill, /rgba|transparent/);
      await page.locator("#timeline").screenshot({
        path: `output/playwright/weekly-quota-timeline-${theme}.png`,
      });
      await page.locator('#allowances [data-open-account="account-a"]').hover();
      await page.waitForFunction(() => {
        const button = document.querySelector(
          '#allowances [data-open-account="account-a"]',
        );
        getComputedStyle(button).backgroundColor;
        return button
          .getAnimations()
          .every((animation) => animation.playState !== "running");
      });
      const contrast = await page
        .locator('#allowances [data-open-account="account-a"]')
        .evaluate((button) => {
          const luminance = (colour) => {
            const v = colour
              .match(/\d+(?:\.\d+)?/g)
              .slice(0, 3)
              .map(Number)
              .map((n) => {
                const x = n / 255;
                return x <= 0.04045
                  ? x / 12.92
                  : Math.pow((x + 0.055) / 1.055, 2.4);
              });
            return 0.2126 * v[0] + 0.7152 * v[1] + 0.0722 * v[2];
          };
          const s = getComputedStyle(button),
            a = luminance(s.color),
            b = luminance(s.backgroundColor);
          return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
        });
      hoverReadable &&= contrast >= 4.5;
    }
    await page.locator('#allowances [data-open-account="account-a"]').focus();
    await page.keyboard.press("Enter");
    const clickWorks = await page.locator("#accounts-section").isVisible();
    assert.deepEqual(
      { axisClear, hoverReadable, clickWorks },
      { axisClear: true, hoverReadable: true, clickWorks: true },
    );
    assert.equal(
      await page.locator("#accounts article").getAttribute("data-account"),
      "account-a",
    );
    await page.getByRole("searchbox", { name: "Find account" }).fill("");
    await page.getByRole("button", { name: "Overview", exact: true }).click();
    await later.locator(".timeline-later").focus();
    await page.keyboard.press("Enter");
    assert.equal(
      await page.locator("#accounts article").getAttribute("data-account"),
      "account-b",
    );
    assert.match(
      await page.locator("#accounts").textContent(),
      /22\/10|Oct 22|22 Oct/,
    );
    const after = await readState();
    for (const key of ["writes", "settings_writes", "provider_refreshes"])
      assert.equal(after[key], before[key]);
    assert.deepEqual(errors, []);
    await page.getByRole("button", { name: "Disconnect", exact: true }).click();
    assert.equal(
      await page.locator("#allowances .allowance-account").count(),
      0,
    );
    console.log(
      "PASS: proportional opaque quota fill, seven-day horizon, later expiries in account details, elapsed strip, separate expiry lines, mobile/theme geometry, keyboard navigation, no writes",
    );
  } finally {
    await browser.close();
  }
})().catch((e) => {
  console.error(e);
  process.exitCode = 1;
});
