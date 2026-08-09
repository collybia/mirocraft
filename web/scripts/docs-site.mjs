/*
 * Builds the documentation site out of the Markdown that already lives in the
 * repository.
 *
 * The docs are written for people reading them in the repository, and they are
 * the same docs a released project needs on a page. Keeping one copy and
 * rendering it means the site cannot drift from what the maintainers read;
 * keeping two means the site is wrong within a release.
 *
 * Everything is self-contained: no CDN, no font, no analytics. A page about a
 * self-hosted panel that phones home while explaining privacy would be funny
 * for one reading and embarrassing after.
 *
 * Usage:
 *   node scripts/docs-site.mjs <out-dir>
 */

import {
  copyFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { marked } from "marked";

const here = dirname(fileURLToPath(import.meta.url));
const repo = resolve(here, "..", "..");
const outDir = resolve(process.argv[2] ?? join(repo, "site"));

/** The site, in the order the sidebar shows it. */
const PAGES = [
  {
    file: "README.md",
    out: "index.html",
    title: "Mirocraft",
    nav: "Обзор",
    description: "Панель управления Minecraft-серверами, которую ставят себе.",
  },
  {
    file: "docs/API_GUIDE.md",
    out: "api-guide.html",
    title: "API — гайд",
    nav: "API: гайд",
    description: "Как управлять серверами из скрипта: примеры curl, проверенные на живой панели.",
  },
  {
    file: "docs/API.md",
    out: "api.html",
    title: "API — справочник",
    nav: "API: справочник",
    description: "Все эндпоинты Mirocraft и объяснение, почему они такие.",
  },
  {
    file: "docs/ARCHITECTURE.md",
    out: "architecture.html",
    title: "Архитектура",
    nav: "Архитектура",
    description: "Как устроен демон, runner, ядра, панель и боты.",
  },
  {
    file: "docs/SECURITY.md",
    out: "security.html",
    title: "Безопасность",
    nav: "Безопасность",
    description: "Модель угроз, что защищено и как сообщить об уязвимости.",
  },
  {
    file: "docs/ROADMAP.md",
    out: "roadmap.html",
    title: "Что сделано",
    nav: "Что сделано",
    description: "План по фазам и то, что нашлось при проверке каждой.",
  },
  {
    file: "README.en.md",
    out: "en.html",
    title: "Mirocraft in English",
    nav: "English",
    description: "A Minecraft server control panel you host yourself.",
  },
];

/** Markdown links that have to become links between pages of the site. */
const LINK_MAP = new Map([
  ["README.md", "index.html"],
  ["README.en.md", "en.html"],
  ["docs/API.md", "api.html"],
  ["docs/API_GUIDE.md", "api-guide.html"],
  ["docs/ARCHITECTURE.md", "architecture.html"],
  ["docs/SECURITY.md", "security.html"],
  ["docs/ROADMAP.md", "roadmap.html"],
  ["API.md", "api.html"],
  ["API_GUIDE.md", "api-guide.html"],
  ["ARCHITECTURE.md", "architecture.html"],
  ["SECURITY.md", "security.html"],
  ["ROADMAP.md", "roadmap.html"],
]);

const REPO_URL = "https://github.com/collybia/mirocraft";

/** Headings collected while rendering, for the page's own contents list. */
let headings = [];
const seenIds = new Set();

/**
 * slug builds an anchor from a heading.
 *
 * Cyrillic is transliterated rather than dropped: an id of "" for every
 * Russian heading would collide with itself on every page.
 */
function slug(text) {
  const map = {
    а: "a", б: "b", в: "v", г: "g", д: "d", е: "e", ё: "e", ж: "zh", з: "z",
    и: "i", й: "y", к: "k", л: "l", м: "m", н: "n", о: "o", п: "p", р: "r",
    с: "s", т: "t", у: "u", ф: "f", х: "h", ц: "c", ч: "ch", ш: "sh", щ: "sch",
    ъ: "", ы: "y", ь: "", э: "e", ю: "yu", я: "ya",
  };

  let base = text
    .toLowerCase()
    .replace(/[а-яё]/g, (ch) => map[ch] ?? "")
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  if (!base) base = "section";

  let id = base;
  for (let n = 2; seenIds.has(id); n++) id = `${base}-${n}`;
  seenIds.add(id);
  return id;
}

function escapeHtml(text) {
  return text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

/** rewriteLink turns a repository path into the page it became. */
function rewriteLink(href) {
  if (!href) return href;
  if (/^(https?:|mailto:|#)/.test(href)) return href;

  const [path, hash = ""] = href.split("#");
  const mapped = LINK_MAP.get(path.replace(/^\.\//, ""));
  if (mapped) return hash ? `${mapped}#${hash}` : mapped;

  // Screenshots travel with the site; everything else the docs point at is a
  // file in the repository, so it is linked where it actually is. That covers
  // the bare names too — mirocraft.example.yaml is a file, not a page, and
  // leaving it relative made it a link into nothing.
  if (path.startsWith("docs/screenshots/")) return path.replace(/^docs\//, "");
  return `${REPO_URL}/blob/master/${path}`;
}

marked.use({
  gfm: true,
  renderer: {
    heading({ tokens, depth }) {
      const text = this.parser.parseInline(tokens);
      const plain = tokens.map((t) => t.raw ?? "").join("");
      const id = slug(plain);
      if (depth === 2 || depth === 3) headings.push({ id, depth, text });
      // The anchor is the heading itself: a separate link glyph is one more
      // thing to style and one more thing to miss on a phone.
      return `<h${depth} id="${id}"><a class="anchor" href="#${id}">${text}</a></h${depth}>\n`;
    },
    link({ href, title, tokens }) {
      const text = this.parser.parseInline(tokens);
      const target = rewriteLink(href);
      const external = /^https?:/.test(target);
      const attrs = [
        `href="${escapeHtml(target)}"`,
        title ? `title="${escapeHtml(title)}"` : "",
        external ? 'target="_blank" rel="noreferrer noopener"' : "",
      ].filter(Boolean);
      return `<a ${attrs.join(" ")}>${text}</a>`;
    },
    image({ href, title, text }) {
      const src = rewriteLink(href);
      return `<img src="${escapeHtml(src)}" alt="${escapeHtml(text ?? "")}"${
        title ? ` title="${escapeHtml(title)}"` : ""
      } loading="lazy">`;
    },
  },
});

/**
 * screenshotGrid turns a table of images into a figure grid.
 *
 * The README lays its screenshots out as a two-column table, which is what
 * GitHub renders well. As a table on a page it is a horizontally scrolling
 * box with the images squeezed into whatever width the captions leave — so
 * the markup is replaced rather than fought with CSS.
 *
 * The input is this generator's own output for a known table, not arbitrary
 * HTML, which is why matching it with a pattern is safe here.
 */
function screenshotGrid(html) {
  return html.replace(/<table>[\s\S]*?<\/table>/g, (table) => {
    if (!table.includes("<img")) return table;

    // By row and by column, not by "the next cell without an image": the
    // table is a row of pictures above a row of captions, so the caption for
    // the second picture is the second cell of the row below — taking the
    // first free one gives every picture the same caption, off by one.
    const rows = [...table.matchAll(/<tr>([\s\S]*?)<\/tr>/g)].map((row) =>
      [...row[1].matchAll(/<t[dh][^>]*>([\s\S]*?)<\/t[dh]>/g)].map((c) => c[1].trim()),
    );

    const figures = [];
    rows.forEach((row, index) => {
      if (!row.some((cell) => cell.includes("<img"))) return;
      const below = rows[index + 1];
      const captions = below && !below.some((cell) => cell.includes("<img")) ? below : [];

      row.forEach((cell, column) => {
        if (!cell.includes("<img")) return;
        const caption = captions[column] ?? "";
        figures.push(
          `<figure>${cell}${caption ? `<figcaption>${caption}</figcaption>` : ""}</figure>`,
        );
      });
    });
    if (figures.length === 0) return table;
    return `<div class="shots">\n${figures.join("\n")}\n</div>`;
  });
}

function navHtml(current) {
  const items = PAGES.map((page) => {
    const active = page.out === current ? ' class="active"' : "";
    return `<a href="${page.out}"${active}>${page.nav}</a>`;
  }).join("\n        ");

  return `      <nav class="pages">
        ${items}
      </nav>`;
}

function tocHtml() {
  if (headings.length < 3) return "";
  const items = headings
    .map((h) => `<a class="d${h.depth}" href="#${h.id}">${h.text}</a>`)
    .join("\n        ");
  return `      <nav class="toc">
        <p class="toc-title">На этой странице</p>
        ${items}
      </nav>`;
}

function layout({ title, description, body, current }) {
  const heading = title === "Mirocraft" ? "Mirocraft" : `${title} — Mirocraft`;
  return `<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${escapeHtml(heading)}</title>
<meta name="description" content="${escapeHtml(description)}">
<link rel="stylesheet" href="styles.css">
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'><rect width='7' height='7' x='1' y='1' fill='%2334d17f'/><rect width='5' height='5' x='9' y='2' fill='%2334d17f'/><rect width='5' height='5' x='2' y='9' fill='%2334d17f'/></svg>">
</head>
<body>
<header class="top">
  <a class="brand" href="index.html">Mirocraft</a>
  <a class="repo" href="${REPO_URL}" target="_blank" rel="noreferrer noopener">GitHub</a>
</header>

<div class="shell">
  <aside class="side">
${navHtml(current)}
${tocHtml()}
  </aside>

  <main>
${body}
  </main>
</div>

<footer>
  <p>Документация собирается из Markdown этого же репозитория —
  <a href="${REPO_URL}/tree/master/docs" target="_blank" rel="noreferrer noopener">docs/</a>.
  Нашли ошибку — правьте файл, страница пересоберётся сама.</p>
</footer>
</body>
</html>
`;
}

const STYLES = `/* The palette is the panel's own, so the docs and the product look like
   one thing. Two themes, chosen by the reader's system setting: a docs site
   has no login to remember a preference in. */
:root {
  --bg: #ffffff;
  --bg-elevated: #f5f6f8;
  --text: #1a1d21;
  --text-muted: #5a6672;
  --text-faint: #8a949e;
  --accent: #0f9d58;
  --border: #dfe3e8;
  --code-bg: #f2f4f7;
  --warning: #8a6100;
}

@media (prefers-color-scheme: dark) {
  :root {
    --bg: #16181c;
    --bg-elevated: #1e2127;
    --text: #e6e9ee;
    --text-muted: #9aa4b0;
    --text-faint: #6d7783;
    --accent: #34d17f;
    --border: #2a2f37;
    --code-bg: #1b1e24;
    --warning: #e2b341;
  }
}

* { box-sizing: border-box; }

body {
  margin: 0;
  background: var(--bg);
  color: var(--text);
  font: 16px/1.65 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
}

a { color: var(--accent); text-decoration: none; }
a:hover { text-decoration: underline; }

.top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 1rem;
  padding: 0.85rem 1.5rem;
  border-bottom: 1px solid var(--border);
  position: sticky;
  top: 0;
  background: var(--bg);
  z-index: 10;
}

.brand { font-weight: 600; color: var(--text); font-size: 1.05rem; }
.repo { color: var(--text-muted); font-size: 0.9rem; }

.shell {
  display: grid;
  grid-template-columns: 15rem minmax(0, 1fr);
  gap: 2.5rem;
  max-width: 68rem;
  margin: 0 auto;
  padding: 2rem 1.5rem 4rem;
}

.side { position: sticky; top: 4rem; align-self: start; max-height: calc(100vh - 6rem); overflow-y: auto; }
.side nav { display: flex; flex-direction: column; gap: 0.15rem; }
.pages a { color: var(--text-muted); padding: 0.3rem 0.6rem; border-radius: 4px; }
.pages a:hover { background: var(--bg-elevated); text-decoration: none; }
.pages a.active { color: var(--text); background: var(--bg-elevated); font-weight: 500; }

.toc { margin-top: 1.75rem; padding-top: 1rem; border-top: 1px solid var(--border); }
.toc-title { margin: 0 0 0.5rem; color: var(--text-faint); font-size: 0.78rem; text-transform: uppercase; letter-spacing: 0.05em; }
.toc a { color: var(--text-muted); font-size: 0.88rem; padding: 0.15rem 0.6rem; }
.toc a.d3 { padding-left: 1.5rem; font-size: 0.84rem; }

main { min-width: 0; }
main h1 { font-size: 2rem; line-height: 1.25; margin: 0 0 1.25rem; }
main h2 { font-size: 1.4rem; margin: 2.5rem 0 0.75rem; padding-top: 0.5rem; }
main h3 { font-size: 1.1rem; margin: 1.75rem 0 0.5rem; }
main h2 + h3 { margin-top: 1rem; }
main .anchor { color: inherit; }
main .anchor:hover { text-decoration: none; color: var(--accent); }

main p, main li { color: var(--text); }
main blockquote { margin: 1rem 0; padding: 0.5rem 1rem; border-left: 3px solid var(--border); color: var(--text-muted); }

code {
  font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
  font-size: 0.88em;
  background: var(--code-bg);
  padding: 0.12em 0.35em;
  border-radius: 3px;
}

pre {
  background: var(--code-bg);
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 0.9rem 1rem;
  overflow-x: auto;
}
pre code { background: none; padding: 0; font-size: 0.85rem; line-height: 1.55; }

table { border-collapse: collapse; width: 100%; margin: 1rem 0; font-size: 0.92rem; display: block; overflow-x: auto; }
th, td { border: 1px solid var(--border); padding: 0.45rem 0.7rem; text-align: left; vertical-align: top; }
th { background: var(--bg-elevated); font-weight: 600; }

img { max-width: 100%; height: auto; border-radius: 6px; border: 1px solid var(--border); }
hr { border: none; border-top: 1px solid var(--border); margin: 2.5rem 0; }

/* The screenshots: a grid of figures, built from the README's table by the
   generator. A table would scroll sideways and squeeze the pictures into
   whatever width the captions left over. */
.shots { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 1.25rem; margin: 1.25rem 0 2rem; }
.shots figure { margin: 0; }
.shots img { width: 100%; display: block; }
.shots figcaption { margin-top: 0.45rem; color: var(--text-muted); font-size: 0.85rem; line-height: 1.4; }

footer {
  border-top: 1px solid var(--border);
  padding: 1.5rem;
  color: var(--text-faint);
  font-size: 0.85rem;
  text-align: center;
}
footer a { color: var(--text-muted); }

@media (max-width: 52rem) {
  .shell { grid-template-columns: minmax(0, 1fr); gap: 1.5rem; padding-top: 1.25rem; }
  .side { position: static; max-height: none; }
  .pages { flex-direction: row; flex-wrap: wrap; }
  .toc { display: none; }
  .shots { grid-template-columns: minmax(0, 1fr); }
}
`;

// --- build ---

mkdirSync(outDir, { recursive: true });

for (const page of PAGES) {
  headings = [];
  seenIds.clear();

  const source = readFileSync(join(repo, page.file), "utf8");
  const body = screenshotGrid(marked.parse(source));

  writeFileSync(
    join(outDir, page.out),
    layout({ title: page.title, description: page.description, body, current: page.out }),
  );
  console.log(`ok   ${page.out}  (${page.file})`);
}

writeFileSync(join(outDir, "styles.css"), STYLES);

// Copied file by file rather than with cpSync: that call crashes this Node
// build outright on Windows — exit 0xC0000409, no exception to catch — and a
// generator that dies after writing half a site is worse than a loop.
const shotsIn = join(repo, "docs", "screenshots");
const shotsOut = join(outDir, "screenshots");
mkdirSync(shotsOut, { recursive: true });
for (const name of readdirSync(shotsIn)) {
  copyFileSync(join(shotsIn, name), join(shotsOut, name));
}

// Without this GitHub Pages runs the output through Jekyll, which drops files
// and directories whose names begin with an underscore.
writeFileSync(join(outDir, ".nojekyll"), "");

console.log(`ok   styles.css, screenshots/, .nojekyll`);

// --- check the links ---
//
// A docs site is mostly links, and the ones that break are the internal ones:
// a heading renamed in Markdown silently becomes a link to nowhere. Checking
// here makes the build fail instead of the reader.

const problems = [];

for (const page of PAGES) {
  const html = readFileSync(join(outDir, page.out), "utf8");
  const ids = new Set([...html.matchAll(/\bid="([^"]+)"/g)].map((m) => m[1]));

  for (const [, raw] of html.matchAll(/(?:href|src)="([^"]+)"/g)) {
    if (/^(https?:|mailto:|data:)/.test(raw)) continue;

    const [path, anchor] = raw.split("#");
    if (path && !existsSync(join(outDir, path))) {
      problems.push(`${page.out}: ${raw} — no such file in the site`);
      continue;
    }
    if (!anchor) continue;

    // An anchor into another page is checked against that page's own ids.
    const targetIds = path
      ? new Set(
          [...readFileSync(join(outDir, path), "utf8").matchAll(/\bid="([^"]+)"/g)].map(
            (m) => m[1],
          ),
        )
      : ids;
    if (!targetIds.has(anchor)) {
      problems.push(`${page.out}: ${raw} — no heading with that anchor`);
    }
  }
}

if (problems.length > 0) {
  console.error(`\n${problems.length} broken link(s):`);
  for (const problem of problems) console.error(`  ${problem}`);
  process.exit(1);
}

console.log("ok   every internal link resolves");
console.log(`site written to ${outDir}`);
