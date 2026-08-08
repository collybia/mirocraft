/*
 * The theme registry.
 *
 * Adding a built-in theme means adding its CSS file and one entry here. No
 * component is touched, because components only ever read tokens.
 */

import "./tokens.css";
import "./dark.css";
import "./light.css";
import "./midnight.css";
import "./grass.css";
import "./nether.css";

export type ThemeBase = "dark" | "light";

export interface BuiltinTheme {
  id: string;
  name: string;
  base: ThemeBase;
  preview: { bg: string; text: string; accent: string };
}

/** Follows the operating system's colour scheme. Default for new accounts. */
export const SYSTEM = "system";

export const BUILTIN_THEMES: BuiltinTheme[] = [
  {
    id: "dark",
    name: "Тёмная",
    base: "dark",
    preview: { bg: "#16181d", text: "#e6e8ee", accent: "#4ade80" },
  },
  {
    id: "light",
    name: "Светлая",
    base: "light",
    preview: { bg: "#ffffff", text: "#1a1d23", accent: "#157f3c" },
  },
  {
    id: "midnight",
    name: "Полночь",
    base: "dark",
    preview: { bg: "#000000", text: "#dbe3f4", accent: "#5b8cff" },
  },
  {
    id: "grass",
    name: "Трава",
    base: "dark",
    preview: { bg: "#161b14", text: "#e4ead9", accent: "#8bc34a" },
  },
  {
    id: "nether",
    name: "Незер",
    base: "dark",
    preview: { bg: "#1c1210", text: "#f0e0d8", accent: "#ff6a3d" },
  },
];

export function findBuiltin(id: string): BuiltinTheme | undefined {
  return BUILTIN_THEMES.find((theme) => theme.id === id);
}

/*
 * Everything below exists so that colour authoring stays in this module.
 *
 * The theme editor and the theme picker genuinely need literal colours — a
 * swatch has to show a theme that is not currently applied, and the editor
 * starts from some default. Keeping those literals here rather than in the
 * components is what lets the colour-literal check stay strict everywhere
 * else.
 */

/** Starting point for a brand-new custom theme. */
export const EDITOR_DEFAULTS = {
  accent: "#7c5cff",
  radiusPx: 10,
} as const;

/** The readable text colour to place on top of the accent, per base. */
export function accentForeground(base: ThemeBase): string {
  return base === "light" ? "#ffffff" : "#0b0b12";
}

/** Fallback swatch colours for a custom theme that overrides only some tokens. */
export function swatchFor(
  base: ThemeBase,
  vars: Record<string, string>,
): { bg: string; text: string; accent: string } {
  const fallback = findBuiltin(base) ?? BUILTIN_THEMES[0];
  return {
    bg: vars["--bg"] ?? fallback.preview.bg,
    text: vars["--text"] ?? fallback.preview.text,
    accent: vars["--accent"] ?? fallback.preview.accent,
  };
}
