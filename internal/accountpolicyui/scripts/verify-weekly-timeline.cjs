/* Reference-layout regressions: sanitized fixture only, no live writes. */
const assert = require("node:assert/strict");
const { chromium } = require(
  process.env.PLAYWRIGHT_MODULE_PATH || "playwright",
);
const base = process.env.ACCOUNT_POLICY_PREVIEW_URL || "http://127.0.0.1:18319";
// Read actual painted pixels, not just SVG attributes (clipping can hide a rect).
function pixel(png, x, y) {
  const { inflateSync } = require("node:zlib");
  const width = png.readUInt32BE(16),
    bpp = png[25] === 6 ? 4 : 3;
  assert.ok([2, 6].includes(png[25]));
  const chunks = [];
  for (let at = 8; at < png.length;) {
    const size = png.readUInt32BE(at);
    if (png.toString("ascii", at + 4, at + 8) === "IDAT")
      chunks.push(png.subarray(at + 8, at + 8 + size));
    at += size + 12;
  }
  const raw = inflateSync(Buffer.concat(chunks)),
    stride = width * bpp;
  let previous = Buffer.alloc(stride),
    cursor = 0;
  const paeth = (a, b, c) => {
    const p = a + b - c,
      aa = Math.abs(p - a),
      bb = Math.abs(p - b),
      cc = Math.abs(p - c);
    return aa <= bb && aa <= cc ? a : bb <= cc ? b : c;
  };
  for (let row = 0; row <= y; row++) {
    const filter = raw[cursor++],
      current = Buffer.alloc(stride);
    for (let i = 0; i < stride; i++) {
      const a = i >= bpp ? current[i - bpp] : 0,
        b = previous[i],
        c = i >= bpp ? previous[i - bpp] : 0;
      const predict = [0, a, b, Math.floor((a + b) / 2), paeth(a, b, c)][
        filter
      ];
      assert.notEqual(predict, undefined);
      current[i] = (raw[cursor++] + predict) & 255;
    }
    previous = current;
  }
  return [...previous.subarray(x * bpp, x * bpp + 3)];
}
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
    const errors = [];
    page.on("pageerror", (e) => errors.push(e.message));
    const state = async () => {
      const response = await page.request.get(base + "/fixture/state", {
        headers: { Authorization: "Bearer fixture-key" },
      });
      assert.equal(response.ok(), true);
      const data = await response.json();
      assert.equal(data.fixture_only, true, "Never run against production");
      return data;
    };
    const before = await state();
    await page.clock.setFixedTime(new Date(before.accounts[0].observed_at));
    await page.goto(base + "/account-policy.html");
    await page
      .getByRole("textbox", { name: "Management key" })
      .fill("fixture-key");
    await page.getByRole("button", { name: "Connect", exact: true }).click();
    await page.locator("#workspace").waitFor({ state: "visible" });
    const weekly = page.getByRole("button", { name: "Weekly", exact: true });
    assert.equal(
      await weekly.count(),
      1,
      "Reference Weekly control is missing",
    );
    assert.equal(await weekly.getAttribute("aria-pressed"), "true");
    assert.equal(await page.locator(".timeline-day").count(), 14);
    const row = page.locator(".timeline-row").filter({
      has: page.locator(".timeline-account", { hasText: "Account A" }),
    });
    const later = page.locator(".timeline-row").filter({
      has: page.locator(".timeline-account", { hasText: "Account B" }),
    });
    assert.match(
      await row.locator(".timeline-cycle-label").textContent(),
      /33% left/,
    );
    assert.equal(await later.locator(".timeline-marker.expiry").count(), 1);
    assert.match(
      await later.locator(".timeline-later").textContent(),
      /2 later expiries/,
    );
    assert.doesNotMatch(
      await page.locator("#timeline").textContent(),
      /22\/10|29\/10/,
    );
    assert.equal(await page.locator(".timeline-marker.weekly").count(), 0);
    for (const width of [1440, 1024, 768, 390, 320]) {
      await page.setViewportSize({ width, height: 1000 });
      for (const theme of ["light", "dark"]) {
        await page.locator("#theme").selectOption(theme);
        const g = await row.evaluate((el) => {
          const bar = el
            .querySelector(".weekly-cycle-outline")
            .getBoundingClientRect();
          const elapsed = el
            .querySelector(".weekly-cycle-elapsed")
            .getBoundingClientRect();
          const track = el
            .querySelector(".timeline-weekly")
            .getBoundingClientRect();
          const style = getComputedStyle(
            el.querySelector(".weekly-cycle-elapsed"),
          );
          return {
            elapsed: elapsed.width / bar.width,
            window: bar.width / track.width,
            height: bar.height,
            font: parseFloat(
              getComputedStyle(el.querySelector(".timeline-cycle-label"))
                .fontSize,
            ),
            opacity: style.opacity,
            colour: style.fill.match(/\d+/g).slice(0, 3).map(Number),
            x: Math.round(elapsed.left - track.left + elapsed.width / 2),
            y: Math.round(elapsed.top - track.top + 3),
          };
        });
        assert.ok(
          Math.abs(g.elapsed - 3 / 7) < 0.002,
          "Elapsed time must not equal quota consumed",
        );
        assert.ok(
          Math.abs(g.window - 0.5) < 0.002,
          "Seven days in a fourteen-day calendar",
        );
        assert.equal(g.height, 24);
        assert.equal(g.font, 11, "Label text must not shrink on mobile");
        assert.equal(g.opacity, "1");
        assert.deepEqual(
          pixel(await row.locator(".timeline-weekly").screenshot(), g.x, g.y),
          g.colour,
          "Elapsed segment must actually paint its colour",
        );
        const overlaps = await page
          .locator(".timeline-axis-track")
          .evaluate((svg) => {
            const boxes = [...svg.querySelectorAll("text")]
              .filter((t) => getComputedStyle(t).display !== "none")
              .map((t) => t.getBoundingClientRect());
            return boxes.some((a, i) =>
              boxes
                .slice(i + 1)
                .some(
                  (b) =>
                    a.left < b.right &&
                    b.left < a.right &&
                    a.top < b.bottom &&
                    b.top < a.bottom,
                ),
            );
          });
        assert.equal(overlaps, false, `Date overlap at ${width}px/${theme}`);
        assert.equal(
          await page.evaluate(() => document.documentElement.scrollWidth),
          width,
        );
        assert.equal(
          await page
            .locator("#timeline")
            .evaluate((el) => el.scrollWidth > el.clientWidth),
          false,
        );
        if ([1440, 390].includes(width))
          await page
            .locator("#timeline")
            .locator("..")
            .screenshot({
              path: `output/playwright/quota-windows-${width}-${theme}.png`,
            });
      }
    }
    await page.getByRole("button", { name: "5-hour", exact: true }).click();
    assert.equal(await page.locator(".timeline-day").count(), 12);
    assert.equal(await row.locator(".timeline-duration").textContent(), "5h");
    assert.match(
      await row.locator(".weekly-cycle").getAttribute("aria-label"),
      /Five-hour window/,
    );
    const short = before.accounts[0].buckets.find(
      (b) => b.duration_seconds === 18000,
    );
    assert.match(
      await row.locator(".timeline-cycle-label").textContent(),
      new RegExp(`${100 - short.used_percent}% left`),
    );
    await page
      .locator("#timeline")
      .locator("..")
      .screenshot({ path: "output/playwright/quota-windows-5hour-mobile.png" });
    await weekly.click();
    const period = await page.locator("#timeline-period").textContent();
    await page.getByRole("button", { name: "Next timeline period" }).click();
    assert.notEqual(
      await page.locator("#timeline-period").textContent(),
      period,
    );
    assert.equal(
      await row.locator(".weekly-cycle").count(),
      0,
      "Never invent observed future cycles",
    );
    assert.equal(
      await page.locator(".timeline-now").count(),
      0,
      "Never clamp Now to an edge",
    );
    await page
      .getByRole("button", { name: "Previous timeline period" })
      .click();
    assert.equal(await page.locator("#timeline-period").textContent(), period);
    await page
      .getByRole("button", { name: "Previous timeline period" })
      .click();
    await page.getByRole("button", { name: "Today", exact: true }).click();
    assert.equal(await page.locator("#timeline-period").textContent(), period);
    await page.setViewportSize({ width: 1440, height: 1000 });
    for (const zone of ["UTC", "GMT+2", "Europe/Berlin"]) {
      await page.locator("#zone").selectOption(zone);
      assert.equal(await row.locator(".timeline-now").count(), 1);
      assert.equal(await page.locator(".timeline-day.today").count(), 1);
    }
    const cycle = row.locator(".weekly-cycle");
    await cycle.focus();
    const focusKey = await cycle.getAttribute("data-focus-key");
    await page.waitForResponse((r) => r.url().includes("/dashboard?range="));
    assert.equal(
      await page.evaluate(() => document.activeElement.dataset.focusKey),
      focusKey,
    );
    await later.locator(".timeline-later").focus();
    await page.waitForResponse((r) => r.url().includes("/dashboard?range="));
    assert.equal(
      await page.evaluate(() =>
        document.activeElement.classList.contains("timeline-later"),
      ),
      true,
      "Later-expiry focus must survive a background refresh",
    );
    await page.keyboard.press("Enter");
    assert.equal(
      await page.locator("#accounts article").getAttribute("data-account"),
      "account-b",
    );
    const after = await state();
    for (const key of ["writes", "settings_writes", "provider_refreshes"])
      assert.equal(after[key], before[key]);
    assert.deepEqual(errors, []);
    console.log(
      "PASS: reference calendar grid, exact time/quota labels, mobile/theme geometry, mode/navigation, keyboard focus, no writes",
    );
  } finally {
    await browser.close();
  }
})().catch((e) => {
  console.error(e);
  process.exitCode = 1;
});
