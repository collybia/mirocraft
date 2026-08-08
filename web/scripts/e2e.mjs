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

  /* --- files ---
   *
   * A server that has never started still has eula.txt, written when it was
   * created. That is enough: the point is that the tab talks to the sandbox,
   * not that a world exists.
   */

  await page.goto(baseURL, { waitUntil: "networkidle" });
  await page.click("text=e2e-test");
  await page.waitForSelector("text=Консоль", { timeout: 20000 });
  await page.click('button:has-text("Файлы")');
  await page.waitForSelector('button:has-text("eula.txt")', { timeout: 20000 });
  check("the files tab reads the server directory", true);

  page.once("dialog", (d) => d.accept("plugins"));
  await page.click('button:has-text("Новый каталог")');
  await page.waitForSelector('button:has-text("plugins")', { timeout: 20000 });
  check("a directory can be created", true);

  await page.click('button:has-text("plugins")');
  await page.waitForSelector('button:has-text("↑ наверх")', { timeout: 20000 });
  check("a directory can be entered", true);
  await page.click('button:has-text("↑ наверх")');
  await page.waitForTimeout(300);

  await page.setInputFiles('input[type="file"]', {
    name: "notes.txt",
    mimeType: "text/plain",
    buffer: Buffer.from("hello from e2e\n"),
  });
  await page.waitForSelector('button:has-text("notes.txt")', { timeout: 20000 });
  check("a file can be uploaded", true);

  await page.click('button:has-text("notes.txt")');
  await page.waitForSelector("textarea", { timeout: 20000 });
  check("an uploaded file opens in the editor",
    (await page.locator("textarea").inputValue()).includes("hello from e2e"));

  await page.locator("textarea").fill("edited by e2e\n");
  await page.click('button:has-text("Сохранить")');
  await page.waitForSelector("text=сохранён", { timeout: 20000 });

  await page.click('button:has-text("notes.txt")');
  await page.waitForSelector("textarea", { timeout: 20000 });
  check("an edit survives a round trip",
    (await page.locator("textarea").inputValue()).includes("edited by e2e"));
  await page.click('button:has-text("Отмена")');

  await shoot(page, "10-files");

  /* --- server.properties ---
   *
   * A server that has never started has no server.properties, so this only
   * asserts the page says so rather than breaking. The full editor is covered
   * against a started server elsewhere.
   */

  await page.click('button:has-text("server.properties")');
  await page.waitForTimeout(1000);
  check("the settings tab does not break without a properties file",
    (await page.locator("textarea, .card").count()) > 0);
  await shoot(page, "11-settings");

  /* --- add-on catalogue ---
   *
   * The search itself needs Modrinth, which a CI box may not be able to
   * reach, so what is asserted here is everything up to that: the tab knows
   * what the server's core accepts. A search that fails offline should not
   * fail the whole run.
   */

  await page.click('button:has-text("Дополнения")');
  await page.waitForSelector("text=Установлено", { timeout: 20000 });
  check("the catalogue tab reports what this core accepts",
    (await page.locator("text=paper ·").count()) > 0);
  check("a server with nothing installed says so",
    (await page.locator("text=Пока ничего не установлено").count()) > 0);
  await shoot(page, "12-catalog");

  /* --- backups and options ---
   *
   * A server that has never started has almost nothing to archive, which is
   * fine: what is checked is that the tab talks to the backup manager and that
   * a schedule round-trips.
   */

  await page.click('button:has-text("Бэкапы")');
  await page.waitForSelector("text=Расписание", { timeout: 20000 });
  check("the backups tab opens", true);

  await page.click('button:has-text("Каждые 6 часов")');
  await page.locator("#backup-keep").fill("3");
  await page.click('button:has-text("Сохранить расписание")');
  await page.waitForSelector("text=Расписание сохранено", { timeout: 20000 });
  check("a backup schedule can be saved", true);
  await shoot(page, "13-backups");

  await page.click('button:has-text("Параметры")');
  await page.waitForSelector("#opt-ram", { timeout: 20000 });
  await page.locator("#opt-ram").fill("1536");
  await page.click('button:has-text("Сохранить")');
  await page.waitForSelector("text=Сохранено", { timeout: 20000 });

  await page.reload({ waitUntil: "networkidle" });
  await page.click('button:has-text("Параметры")');
  await page.waitForSelector("#opt-ram", { timeout: 20000 });
  check("a server option survives a reload",
    (await page.locator("#opt-ram").inputValue()) === "1536");
  await shoot(page, "14-options");

  /* --- administration --- */

  await page.click('a:has-text("Пользователи")');
  await page.waitForSelector("text=Новая учётная запись", { timeout: 20000 });
  check("the admin page lists accounts", (await page.locator("text=это вы").count()) > 0);

  const login = `e2e-user-${Date.now()}`;
  await page.fill("#new-email", login);
  await page.fill("#new-password", "a-long-enough-password");
  await page.click('button:has-text("Создать")');
  await page.waitForSelector(`tr:has-text("${login}")`, { timeout: 20000 });
  check("an account can be created", true);

  await page.locator("tr", { hasText: login }).first()
    .locator('button:has-text("Заблокировать")').click();
  await page.waitForTimeout(1000);
  check("an account can be blocked",
    (await page.locator("tr", { hasText: login }).first()
      .locator("text=заблокирован").count()) > 0);

  page.once("dialog", (d) => d.accept());
  await page.locator("tr", { hasText: login }).first()
    .locator('button:has-text("Удалить")').click();
  await page.waitForTimeout(1200);
  // The row, not the text: the "created" notice still carries the login.
  check("and deleted", (await page.locator("tr", { hasText: login }).count()) === 0);
  await shoot(page, "15-admin");

  check("the page produced no uncaught errors", consoleErrors.length === 0, consoleErrors.join("; "));

  /* --- API documentation ---
   *
   * The Go tests check that the spec matches the router and that the assets
   * are served. Neither can catch a spec that Swagger UI itself refuses to
   * render, which is only visible in a browser.
   */

  const docs = await context.newPage();
  const offHost = [];
  docs.on("request", (r) => {
    if (!r.url().startsWith(baseURL) && !r.url().startsWith("data:")) offHost.push(r.url());
  });

  await docs.goto(`${baseURL}/api/v1/docs`, { waitUntil: "networkidle" });
  await docs.waitForSelector(".opblock-tag", { timeout: 20000 });

  check("the docs page renders the spec", (await docs.locator(".info .title").count()) > 0);
  check("the spec has no errors Swagger UI could not resolve",
    (await docs.locator(".errors-wrapper").count()) === 0);

  // Every group expanded, so a path item that renders as nothing is visible.
  const tags = docs.locator(".opblock-tag");
  const tagCount = await tags.count();
  for (let i = 0; i < tagCount; i += 1) {
    await tags.nth(i).click();
  }
  await docs.waitForTimeout(500);

  const operations = await docs.locator(".opblock").count();
  check("the docs page lists the operations", operations > 50, `${operations} rendered`);

  // The single-binary rule covers the documentation too: a page that only
  // renders when a CDN is reachable is the dependency it exists to avoid.
  check("the docs page loads nothing from outside this daemon",
    offHost.length === 0, offHost.join(", "));

  await shoot(docs, "16-api-docs");
  await docs.close();
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
