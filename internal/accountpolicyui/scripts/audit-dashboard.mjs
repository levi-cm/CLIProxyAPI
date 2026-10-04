// Lighthouse snapshots of authenticated fixture UI without persisting credentials.
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";
const require = createRequire(import.meta.url);
const root = process.env.LIGHTHOUSE_MODULE_PATH;
const lighthouse = await import(
  root ? pathToFileURL(root + "/core/index.js").href : "lighthouse"
);
const puppeteer = require(
  process.env.PUPPETEER_MODULE_PATH || "puppeteer-core",
);
const base = process.env.ACCOUNT_POLICY_PREVIEW_URL || "http://127.0.0.1:18318";
const preflight = await fetch(base + "/fixture/state", {
  headers: { Authorization: "Bearer fixture-key" },
});
assert.equal(
  preflight.ok,
  true,
  "Only the isolated fixture preview is allowed",
);
assert.equal((await preflight.json()).fixture_only, true);
const browser = await puppeteer.launch({
  executablePath: process.env.CHROMIUM_PATH || "/usr/bin/chromium",
  headless: true,
  args: ["--no-sandbox"],
});
try {
  const page = await browser.newPage();
  await page.setViewport({ width: 1440, height: 1000 });
  await page.goto(base + "/account-policy.html");
  await page.type("#key", "fixture-key");
  await page.click("#login-form button");
  await page.waitForSelector("#workspace:not([hidden])");
  for (const theme of ["light", "dark"]) {
    await page.select("#theme", theme);
    for (const view of ["overview", "accounts", "routing", "history"]) {
      await page.click(`[data-view=${view}]`);
      if (view === "routing")
        await page.evaluate(() =>
          document.querySelectorAll("#policy details").forEach((el) => {
            el.open = true;
          }),
        );
      const result = await lighthouse.snapshot(page, {
        flags: {
          onlyCategories: ["accessibility"],
          screenEmulation: { disabled: true },
        },
      });
      const failures = Object.values(result.lhr.audits)
        .filter((a) => a.score !== null && a.score < 1)
        .map((a) => ({ id: a.id, title: a.title, items: a.details?.items }));
      console.log(
        JSON.stringify({
          theme,
          view,
          accessibility: result.lhr.categories.accessibility.score,
          failures,
        }),
      );
      assert.equal(
        failures.length,
        0,
        `${theme}/${view} accessibility failures`,
      );
    }
  }
} finally {
  await browser.close();
}
