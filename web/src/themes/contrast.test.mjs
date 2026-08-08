/*
 * Contrast gate for every theme.
 *
 * Parses each theme file, resolves the tokens against the defaults in
 * tokens.css, and checks the pairs that carry meaning. A theme that fails is a
 * theme that ships unreadable text, so this runs in CI and blocks the merge.
 *
 * Run: node src/themes/contrast.test.mjs
 */

import { readFileSync, readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));

/* WCAG AA thresholds. */
const AA_TEXT = 4.5; // body text
const AA_LARGE = 3.0; // large text, and the boundaries of UI controls

/*
 * The pairs that must stay legible. `large: true` means the 3:1 threshold
 * applies: headings, icons and control outlines rather than body copy.
 */
const PAIRS = [
  ["--text", "--bg"],
  ["--text", "--bg-elevated"],
  ["--text", "--bg-inset"],
  ["--text-muted", "--bg"],
  ["--text-muted", "--bg-elevated"],
  ["--text-faint", "--bg", { large: true }],
  ["--accent-fg", "--accent"],
  ["--accent", "--bg", { large: true }],
  ["--success", "--bg", { large: true }],
  ["--success", "--success-bg"],
  ["--warning", "--bg", { large: true }],
  ["--warning", "--warning-bg"],
  ["--danger", "--bg", { large: true }],
  ["--danger", "--danger-bg"],
  ["--info", "--bg", { large: true }],
  ["--info", "--info-bg"],
  ["--border-strong", "--bg", { large: true }],
  ["--console-text", "--console-bg"],
  ["--console-error", "--console-bg"],
  ["--console-warn", "--console-bg"],
  ["--console-info", "--console-bg"],
  ["--console-debug", "--console-bg"],
  ["--console-timestamp", "--console-bg", { large: true }],
];

/* --- colour maths, straight from the WCAG definition --- */

function parseHex(value) {
  const hex = value.trim().replace(/^#/, "");
  const expand = (c) => parseInt(c.length === 1 ? c + c : c, 16);

  if (hex.length === 3 || hex.length === 4) {
    return [expand(hex[0]), expand(hex[1]), expand(hex[2])];
  }
  if (hex.length === 6 || hex.length === 8) {
    return [expand(hex.slice(0, 2)), expand(hex.slice(2, 4)), expand(hex.slice(4, 6))];
  }
  throw new Error(`cannot parse colour "${value}"`);
}

function relativeLuminance([r, g, b]) {
  const channel = (v) => {
    const s = v / 255;
    return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
  };
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
}

function contrastRatio(a, b) {
  const la = relativeLuminance(parseHex(a));
  const lb = relativeLuminance(parseHex(b));
  const [light, dark] = la > lb ? [la, lb] : [lb, la];
  return (light + 0.05) / (dark + 0.05);
}

/* --- token extraction --- */

function parseTokens(css) {
  const tokens = {};
  for (const match of css.matchAll(/(--[a-z0-9-]+)\s*:\s*([^;]+);/gi)) {
    tokens[match[1]] = match[2].trim();
  }
  return tokens;
}

const defaults = parseTokens(readFileSync(join(here, "tokens.css"), "utf8"));

const themeFiles = readdirSync(here)
  .filter((name) => name.endsWith(".css") && name !== "tokens.css")
  .sort();

if (themeFiles.length === 0) {
  console.error("no theme files found");
  process.exit(1);
}

/* --- the check --- */

let failures = 0;
let checks = 0;

for (const file of themeFiles) {
  const theme = file.replace(/\.css$/, "");
  // Themes inherit anything they do not override from tokens.css.
  const tokens = { ...defaults, ...parseTokens(readFileSync(join(here, file), "utf8")) };

  const missing = PAIRS.flatMap(([fg, bg]) => [fg, bg]).filter((name) => !tokens[name]);
  if (missing.length > 0) {
    console.error(`FAIL ${theme}: missing tokens ${[...new Set(missing)].join(", ")}`);
    failures++;
    continue;
  }

  const problems = [];
  for (const [fg, bg, options = {}] of PAIRS) {
    const threshold = options.large ? AA_LARGE : AA_TEXT;
    const ratio = contrastRatio(tokens[fg], tokens[bg]);
    checks++;

    if (ratio < threshold) {
      problems.push(
        `${fg} on ${bg}: ${ratio.toFixed(2)}:1 (${tokens[fg]} on ${tokens[bg]}), need ${threshold}:1`,
      );
    }
  }

  if (problems.length > 0) {
    failures++;
    console.error(`FAIL ${theme}`);
    for (const problem of problems) console.error(`       ${problem}`);
  } else {
    console.log(`ok   ${theme}`);
  }
}

console.log(`\n${checks} pairs checked across ${themeFiles.length} themes`);

if (failures > 0) {
  console.error(`${failures} theme(s) fail WCAG AA`);
  process.exit(1);
}
