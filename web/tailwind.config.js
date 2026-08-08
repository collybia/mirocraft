/*
 * Tailwind is wired so that its colour utilities resolve to theme tokens.
 * `bg-surface` becomes `background-color: var(--bg)`, so a component written
 * with utilities is themed automatically and there is no reason to reach for
 * a literal.
 *
 * The palette below is the ONLY colour vocabulary available: Tailwind's own
 * scales (gray-900, emerald-500, …) are removed, so `bg-gray-900` simply does
 * not compile. That is deliberate — it turns the project rule into something
 * the build enforces rather than something reviewers have to catch.
 */

/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    // Replacing rather than extending: no default palette, no escape hatch.
    colors: {
      transparent: "transparent",
      current: "currentColor",
      inherit: "inherit",

      surface: "var(--bg)",
      elevated: "var(--bg-elevated)",
      inset: "var(--bg-inset)",

      body: "var(--text)",
      muted: "var(--text-muted)",
      faint: "var(--text-faint)",

      accent: "var(--accent)",
      "accent-hover": "var(--accent-hover)",
      "accent-fg": "var(--accent-fg)",

      success: "var(--success)",
      "success-bg": "var(--success-bg)",
      warning: "var(--warning)",
      "warning-bg": "var(--warning-bg)",
      danger: "var(--danger)",
      "danger-bg": "var(--danger-bg)",
      info: "var(--info)",
      "info-bg": "var(--info-bg)",

      line: "var(--border)",
      "line-strong": "var(--border-strong)",

      "console-bg": "var(--console-bg)",
      "console-text": "var(--console-text)",
      "console-error": "var(--console-error)",
      "console-warn": "var(--console-warn)",
      "console-info": "var(--console-info)",
      "console-debug": "var(--console-debug)",
      "console-timestamp": "var(--console-timestamp)",
    },
    borderRadius: {
      none: "0",
      sm: "var(--radius-sm)",
      DEFAULT: "var(--radius)",
      lg: "var(--radius-lg)",
      full: "9999px",
    },
    extend: {
      fontFamily: {
        sans: "var(--font-sans)",
        mono: "var(--font-mono)",
      },
    },
  },
  plugins: [],
};
