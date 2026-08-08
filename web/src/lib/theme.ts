/*
 * Theme application and caching.
 *
 * The server profile is the source of truth across browsers; localStorage is a
 * cache so the pre-render script in index.html can paint the right colours
 * before React exists.
 */

import { BUILTIN_THEMES, SYSTEM, ThemeBase, findBuiltin } from "../themes";
import type { CustomTheme } from "./api";

const CACHE_KEY = "mirocraft.theme";
const CACHE_BASE_KEY = "mirocraft.theme.base";
const CACHE_VARS_KEY = "mirocraft.theme.vars";

const CUSTOM_PREFIX = "custom:";

/** The token names a custom theme may set, mirroring the server whitelist. */
export const THEME_TOKENS = [
  "--bg",
  "--bg-elevated",
  "--bg-inset",
  "--text",
  "--text-muted",
  "--text-faint",
  "--accent",
  "--accent-hover",
  "--accent-fg",
  "--success",
  "--success-bg",
  "--warning",
  "--warning-bg",
  "--danger",
  "--danger-bg",
  "--info",
  "--info-bg",
  "--border",
  "--border-strong",
  "--radius",
  "--radius-sm",
  "--radius-lg",
  "--console-bg",
  "--console-text",
  "--console-error",
  "--console-warn",
  "--console-info",
  "--console-debug",
  "--console-timestamp",
] as const;

export function isCustom(choice: string): boolean {
  return choice.startsWith(CUSTOM_PREFIX);
}

export function customId(choice: string): string {
  return choice.slice(CUSTOM_PREFIX.length);
}

export function customChoice(id: string): string {
  return CUSTOM_PREFIX + id;
}

/** systemBase resolves the operating system preference. */
export function systemBase(): ThemeBase {
  return window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

/**
 * apply paints a theme choice. Custom themes are painted as their base plus
 * inline overrides, which is also what the pre-render script does.
 */
export function apply(choice: string, customThemes: CustomTheme[] = []): void {
  const root = document.documentElement;

  // Clear any previous custom overrides, or switching away from a custom
  // theme would leave its colours behind.
  for (const token of THEME_TOKENS) {
    root.style.removeProperty(token);
  }

  if (choice === SYSTEM) {
    root.setAttribute("data-theme", systemBase());
    cache(choice, systemBase(), null);
    return;
  }

  if (isCustom(choice)) {
    const theme = customThemes.find((t) => t.id === customId(choice));
    if (!theme) {
      // The theme was deleted elsewhere; fall back rather than paint nothing.
      apply("dark", customThemes);
      return;
    }
    root.setAttribute("data-theme", theme.base);
    for (const [name, value] of Object.entries(theme.vars ?? {})) {
      root.style.setProperty(name, value);
    }
    cache(choice, theme.base, theme.vars ?? {});
    return;
  }

  const builtin = findBuiltin(choice) ?? BUILTIN_THEMES[0];
  root.setAttribute("data-theme", builtin.id);
  cache(builtin.id, builtin.base, null);
}

function cache(choice: string, base: ThemeBase | string, vars: Record<string, string> | null): void {
  try {
    localStorage.setItem(CACHE_KEY, choice);
    localStorage.setItem(CACHE_BASE_KEY, base);
    if (vars) {
      localStorage.setItem(CACHE_VARS_KEY, JSON.stringify(vars));
    } else {
      localStorage.removeItem(CACHE_VARS_KEY);
    }
  } catch {
    // Storage can be unavailable in private mode. The theme still applies for
    // this session; only the pre-render shortcut is lost.
  }
}

/** cachedChoice reads what the pre-render script used. */
export function cachedChoice(): string {
  try {
    return localStorage.getItem(CACHE_KEY) ?? SYSTEM;
  } catch {
    return SYSTEM;
  }
}

/**
 * watchSystem re-applies the theme when the OS preference changes, but only
 * while the user is actually following the system.
 */
export function watchSystem(getChoice: () => string, onChange: () => void): () => void {
  const media = window.matchMedia("(prefers-color-scheme: light)");
  const handler = () => {
    if (getChoice() === SYSTEM) onChange();
  };
  media.addEventListener("change", handler);
  return () => media.removeEventListener("change", handler);
}

/** baseOf reports whether a choice currently resolves to a light or dark base. */
export function baseOf(choice: string, customThemes: CustomTheme[] = []): ThemeBase {
  if (choice === SYSTEM) return systemBase();
  if (isCustom(choice)) {
    return (customThemes.find((t) => t.id === customId(choice))?.base as ThemeBase) ?? "dark";
  }
  return findBuiltin(choice)?.base ?? "dark";
}
