/*
 * End-to-end check of the panel against a real daemon.
 *
 * Covers what unit tests cannot: that the built bundle actually boots, that
 * login works through the real API, that every theme paints, and that the
 * theme is applied before the first paint rather than after React mounts.
 *
 * Usage:
 *   node scripts/e2e.mjs <base-url> <email> <password> [screenshot-dir]
 *
 * Exit code is non-zero on the first failure, so it can gate CI.
 */

import { mkdirSync } from "node:fs";
import { chromium } from "playwright";

const [, , baseURL, email, password, shotDir] = process.argv;

if (!baseURL || !email || !password) {
  console.error("usage: node scripts/e2e.mjs <base-url> <email> <password> [screenshot-dir]");
  process.exit(2);
}

if (shotDir) mkdirSync(shotDir, { recursive: true });

const THEMES = ["dark", "light", "midnight", "grass", "nether"];

let failures = 0;

function check(name, condition, detail = "") {
  if (condition) {
    console.log(`ok   ${name}`);
  } else {
    console.error(`FAIL ${name}${detail ? ` — ${detail}` : ""}`);
    failures++;
  }
}

async function shoot(page, name) {
  if (!shotDir) return;
  await page.screenshot({ path: `${shotDir}/${name}.png`, fullPage: false });
}

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
const page = await context.newPage();

const consoleErrors = [];
page.on("pageerror", (err) => consoleErrors.push(err.message));

try {
  /* --- login --- */

  await page.goto(baseURL, { waitUntil: "networkidle" });
  check("login page renders", await page.locator("text=Панель управления серверами").isVisible());
  await shoot(page, "01-login");

  await page.fill("#email", email);
  await page.fill("#password", password);
  await page.click('button[type="submit"]');

  await page.waitForSelector("text=Серверы", { timeout: 15000 });
  check("login succeeds and reaches the server list", true);
  await shoot(page, "02-servers-empty");

  /* --- creating a server --- */

  await page.click("text=Создать сервер");
  await page.fill("#name", "e2e-test");
  await page.fill("#version", "1.21.4");
  await page.check('input[type="checkbox"]');
  await page.click('button:has-text("Создать")');

  await page.waitForSelector("text=e2e-test", { timeout: 15000 });
  check("a server can be created through the panel", true);
  await shoot(page, "03-servers-list");

  /* --- the server page and its console --- */

  await page.click("text=e2e-test");
  await page.waitForSelector("text=Консоль", { timeout: 15000 });
  check("the server page opens with a console", true);
  await shoot(page, "04-server-console");

  /* --- themes --- */

  await page.goto(`${baseURL}/settings`, { waitUntil: "networkidle" });
  await page.waitForSelector("text=Тема оформления", { timeout: 15000 });
  await shoot(page, "05-settings-themes");

  for (const theme of THEMES) {
    const label = {
      dark: "Тёмная",
      light: "Светлая",
      midnight: "Полночь",
      grass: "Трава",
      nether: "Незер",
    }[theme];

    await page.click(`button:has-text("${label}")`);
    await page.waitForTimeout(200);

    const applied = await page.getAttribute("html", "data-theme");
    check(`theme ${theme} applies`, applied === theme, `data-theme is ${applied}`);

    // The background must actually change, not just the attribute.
    const bg = await page.evaluate(() =>
      getComputedStyle(document.body).backgroundColor,
    );
    check(`theme ${theme} paints a background`, bg !== "" && bg !== "rgba(0, 0, 0, 0)", bg);

    await shoot(page, `06-theme-${theme}`);
  }

  /* --- the theme survives a reload, and there is no flash --- */

  await page.click('button:has-text("Полночь")');
  await page.waitForTimeout(300);

  // The check that matters: sample data-theme at the very first script
  // execution, before any stylesheet or React. If the theme were applied by a
  // component, this would still read the markup default.
  const early = await page.evaluate(() => document.documentElement.getAttribute("data-theme"));
  check("theme is set before reload", early === "midnight", `${early}`);

  const probe = await context.newPage();
  let earliest = null;
  await probe.addInitScript(() => {
    // Runs before any page script. document.documentElement does not exist
    // yet, so the value is sampled the moment it does.
    window.__firstTheme = null;
    document.addEventListener("readystatechange", () => {
      if (window.__firstTheme === null) {
        window.__firstTheme = document.documentElement.getAttribute("data-theme");
      }
    });
  });
  await probe.goto(`${baseURL}/settings`, { waitUntil: "domcontentloaded" });
  earliest = await probe.evaluate(() => window.__firstTheme);

  check(
    "no FOUC: the stored theme is applied before the document is ready",
    earliest === "midnight",
    `first observed data-theme was ${earliest}`,
  );

  await probe.waitForSelector("text=Тема оформления", { timeout: 15000 });
  const afterReload = await probe.getAttribute("html", "data-theme");
  check("theme survives a reload", afterReload === "midnight", `${afterReload}`);
  await shoot(probe, "07-after-reload");
  await probe.close();

  /* --- custom theme --- */

  await page.fill("#theme-name", "E2E Purple");
  await page.click('button:has-text("Создать тему")');
  await page.waitForSelector("text=E2E Purple", { timeout: 15000 });
  check("a custom theme can be created", true);
  await shoot(page, "08-custom-theme");

  check("the page produced no uncaught errors", consoleErrors.length === 0, consoleErrors.join("; "));
} catch (err) {
  console.error(`FAIL unexpected error — ${err.message}`);
  await shoot(page, "99-failure");
  failures++;
} finally {
  await browser.close();
}

if (failures > 0) {
  console.error(`\n${failures} check(s) failed`);
  process.exit(1);
}

console.log("\nall checks passed");
