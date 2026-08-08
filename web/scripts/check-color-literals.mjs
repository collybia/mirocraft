/*
 * Guards the project rule that components contain no colour literals.
 *
 * The roadmap called for an ESLint rule. This is a standalone script instead:
 * it needs no ESLint install, no plugin authoring and no config, and it checks
 * exactly one thing. If the project later adopts ESLint for other reasons,
 * this becomes a rule and the script goes away.
 *
 * What it catches in src/**\/*.tsx and *.ts:
 *   - hex colours          #fff, #7c5cff
 *   - rgb()/hsl() calls
 *   - named CSS colours    red, white, rebeccapurple
 *   - Tailwind palette utilities that survived the config, e.g. bg-gray-900
 *
 * Allowed, because they are the sanctioned way to reach a colour:
 *   - var(--token)
 *   - the theme files themselves and the registry's preview swatches
 *
 * Run: node scripts/check-color-literals.mjs
 */

import { readFileSync, readdirSync, statSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const srcDir = join(root, "src");

/*
 * Files that legitimately hold colours: the themes define the tokens, and the
 * registry carries preview swatches for the picker, which cannot come from
 * variables because they must show a theme that is not currently applied.
 */
const ALLOWED = [
  /^src[\\/]themes[\\/]/,
  /^src[\\/]index\.css$/,
];

const NAMED_COLORS = [
  "black", "white", "red", "green", "blue", "yellow", "orange", "purple",
  "pink", "gray", "grey", "cyan", "magenta", "brown", "silver", "gold",
  "navy", "teal", "olive", "maroon", "lime", "aqua", "fuchsia", "indigo",
  "violet", "rebeccapurple", "crimson", "salmon", "khaki", "beige",
];

const CHECKS = [
  { name: "hex colour", pattern: /#[0-9a-fA-F]{3,8}\b/g },
  { name: "rgb()", pattern: /\brgba?\s*\(/g },
  { name: "hsl()", pattern: /\bhsla?\s*\(/g },
  {
    name: "Tailwind palette utility",
    pattern: /\b(?:bg|text|border|ring|fill|stroke|from|to|via)-(?:slate|gray|grey|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)-\d{2,3}\b/g,
  },
  {
    name: "named CSS colour",
    pattern: new RegExp(`\\b(?:color|background|background-color|border-color|fill|stroke)\\s*:\\s*(?:${NAMED_COLORS.join("|")})\\b`, "gi"),
  },
];

function walk(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      out.push(...walk(full));
    } else if (/\.(tsx?|css)$/.test(entry)) {
      out.push(full);
    }
  }
  return out;
}

let violations = 0;
let scanned = 0;

for (const file of walk(srcDir)) {
  const rel = relative(root, file);
  if (ALLOWED.some((pattern) => pattern.test(rel))) continue;

  scanned++;
  const source = readFileSync(file, "utf8");
  const lines = source.split("\n");

  lines.forEach((line, index) => {
    // A line that reaches a token is fine even if it mentions a colour word.
    if (line.includes("var(--")) return;
    // Skip comments, which often name colours while explaining them.
    const trimmed = line.trim();
    if (trimmed.startsWith("//") || trimmed.startsWith("*") || trimmed.startsWith("/*")) return;

    for (const check of CHECKS) {
      check.pattern.lastIndex = 0;
      const match = check.pattern.exec(line);
      if (match) {
        console.error(`${rel}:${index + 1}  ${check.name}: ${match[0]}`);
        console.error(`    ${trimmed}`);
        violations++;
      }
    }
  });
}

console.log(`\n${scanned} files scanned`);

if (violations > 0) {
  console.error(
    `\n${violations} colour literal(s) found. Components must use theme tokens:\n` +
      `  - Tailwind utilities bound to tokens (bg-surface, text-body, border-line)\n` +
      `  - or var(--token) directly\n` +
      `If no token fits, add one to src/themes/tokens.css rather than an exception.`,
  );
  process.exit(1);
}

console.log("no colour literals outside the theme files");
