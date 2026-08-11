/*
 * The screenshots in the README, taken from a real panel.
 *
 * Not decoration: a screenshot drawn in a mockup tool ages into a lie the
 * first time the layout changes, and there is no way to tell by looking. This
 * drives the built panel against a real daemon, so what lands in docs/ is what
 * the software actually renders.
 *
 * The daemon, the server and the modpack are the caller's job — point this at
 * a panel that already has something worth photographing.
 *
 * Usage:
 *   node scripts/shots.mjs <base-url> <email> <password> <out-dir>
 */

import { mkdirSync } from "node:fs";
import { chromium } from "playwright";

const [, , baseURL, email, password, outDir] = process.argv;

if (!baseURL || !email || !password || !outDir) {
  console.error("usage: node scripts/shots.mjs <base-url> <email> <password> <out-dir>");
  process.exit(2);
}

mkdirSync(outDir, { recursive: true });

// Wide enough that the layout is the desktop one. The height is set per shot:
// a README image padded with empty page reads as an empty product.
const WIDTH = 1440;

const browser = await chromium.launch();
const context = await browser.newContext({
  viewport: { width: WIDTH, height: 820 },
  deviceScaleFactor: 2,
});
const page = await context.newPage();

async function shoot(name, height) {
  await page.setViewportSize({ width: WIDTH, height });
  await page.waitForTimeout(300);
  await page.screenshot({ path: `${outDir}/${name}.png` });
  console.log(`ok   ${name}.png`);
}

try {
  await page.goto(baseURL, { waitUntil: "networkidle" });
  await page.fill("#email", email);
  await page.fill("#password", password);
  await page.click('button[type="submit"]');
  await page.waitForSelector("text=Серверы", { timeout: 20000 });

  // Fixed rather than left to the machine's own preference: the README should
  // look the same whoever regenerates it.
  await page.goto(`${baseURL}/settings`, { waitUntil: "networkidle" });
  await page.waitForSelector("text=Тема оформления", { timeout: 20000 });
  await page.click('button:has-text("Тёмная")');
  await page.waitForTimeout(400);
  await shoot("themes", 700);

  await page.goto(baseURL, { waitUntil: "networkidle" });
  // Long enough for the list to poll more than once: the load trace on a
  // running server needs a second sample before there is a line to draw, and
  // a README image of an empty chart is a README image of a missing feature.
  await page.waitForTimeout(14000);
  await shoot("servers", 560);

  await page.locator("a", { hasText: "Выживание" }).first().click();
  await page.waitForSelector("text=Консоль", { timeout: 20000 });
  // The console fills as the log streams in; a shot taken immediately catches
  // an empty box.
  await page.waitForTimeout(2500);
  await shoot("console", 820);

  // Stopped before the modpacks tab: a running server is refused an install,
  // and a screenshot of the panel explaining why is not what a README is for.
  await page.click('button:has-text("Остановить")');
  await page.waitForSelector("text=остановлен", { timeout: 60000 });
  await page.waitForTimeout(1000);

  await page.click('button:has-text("Модпаки")');
  await page.waitForTimeout(800);
  await page.fill('input[placeholder*="All the Mods"]', "vanilla");
  await page.waitForSelector("text=загрузок", { timeout: 20000 });
  // The icons come from Modrinth's CDN one by one; without this the shot
  // catches a list of grey squares.
  await page.waitForTimeout(2500);
  await shoot("modpacks", 820);
} finally {
  await context.close();
  await browser.close();
}
