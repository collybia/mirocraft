/*
 * Recreates web/dist/.gitkeep after a build.
 *
 * web/embed.go embeds web/dist, so the directory must exist for the Go build
 * to compile at all — including on a clean clone, before anyone has run a
 * frontend build. A committed placeholder solves that, but Vite's emptyOutDir
 * wipes the directory on every build and takes the placeholder with it.
 *
 * Recreating it here keeps both properties: builds start from a clean
 * directory, and the placeholder is always present afterwards.
 */

import { writeFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const dist = join(dirname(fileURLToPath(import.meta.url)), "..", "dist");

mkdirSync(dist, { recursive: true });
writeFileSync(join(dist, ".gitkeep"), "");
