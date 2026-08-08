import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

import * as api from "./lib/api";
import * as theme from "./lib/theme";
import { SYSTEM } from "./themes";

interface ThemeContextValue {
  /** The stored choice: a built-in id, "system", or "custom:<id>". */
  choice: string;
  /** Whether the choice currently resolves to a light or dark palette. */
  base: "dark" | "light";
  customThemes: api.CustomTheme[];
  setChoice: (choice: string) => Promise<void>;
  toggleLightDark: () => Promise<void>;
  reloadCustomThemes: () => Promise<void>;
  /** Paints a theme without saving it — used by the live preview in the editor. */
  preview: (vars: Record<string, string>, base: "dark" | "light") => void;
  endPreview: () => void;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

export function useTheme(): ThemeContextValue {
  const value = useContext(ThemeContext);
  if (!value) throw new Error("useTheme must be used inside ThemeProvider");
  return value;
}

interface Props {
  children: ReactNode;
  /** The profile's theme, once known. Undefined until the user is loaded. */
  serverChoice?: string;
  authenticated: boolean;
}

export function ThemeProvider({ children, serverChoice, authenticated }: Props) {
  // Start from the cache the pre-render script already used, so the first
  // render agrees with the painted page and nothing flashes.
  const [choice, setChoiceState] = useState<string>(() => theme.cachedChoice());
  const [customThemes, setCustomThemes] = useState<api.CustomTheme[]>([]);
  const choiceRef = useRef(choice);
  choiceRef.current = choice;

  const reloadCustomThemes = useCallback(async () => {
    if (!authenticated) return;
    try {
      setCustomThemes(await api.listCustomThemes());
    } catch {
      // A failure here only costs the custom themes; the built-ins still work.
    }
  }, [authenticated]);

  useEffect(() => {
    void reloadCustomThemes();
  }, [reloadCustomThemes]);

  // The profile wins over the cache: it is what follows the user between
  // browsers. Applying it here is what reconciles a choice made elsewhere.
  useEffect(() => {
    if (!serverChoice || serverChoice === choiceRef.current) return;
    setChoiceState(serverChoice);
  }, [serverChoice]);

  useEffect(() => {
    theme.apply(choice, customThemes);
  }, [choice, customThemes]);

  useEffect(
    () =>
      theme.watchSystem(
        () => choiceRef.current,
        () => theme.apply(SYSTEM, customThemes),
      ),
    [customThemes],
  );

  const setChoice = useCallback(
    async (next: string) => {
      setChoiceState(next);
      theme.apply(next, customThemes);

      if (!authenticated) return;
      try {
        await api.updateMe({ theme: next });
      } catch {
        // The theme still applies locally; it just will not follow the user to
        // another browser until the next successful save.
      }
    },
    [authenticated, customThemes],
  );

  const toggleLightDark = useCallback(async () => {
    const current = theme.baseOf(choiceRef.current, customThemes);
    await setChoice(current === "dark" ? "light" : "dark");
  }, [customThemes, setChoice]);

  const preview = useCallback((vars: Record<string, string>, base: "dark" | "light") => {
    const root = document.documentElement;
    root.setAttribute("data-theme", base);
    for (const token of theme.THEME_TOKENS) root.style.removeProperty(token);
    for (const [name, value] of Object.entries(vars)) root.style.setProperty(name, value);
  }, []);

  const endPreview = useCallback(() => {
    theme.apply(choiceRef.current, customThemes);
  }, [customThemes]);

  const value = useMemo<ThemeContextValue>(
    () => ({
      choice,
      base: theme.baseOf(choice, customThemes),
      customThemes,
      setChoice,
      toggleLightDark,
      reloadCustomThemes,
      preview,
      endPreview,
    }),
    [choice, customThemes, setChoice, toggleLightDark, reloadCustomThemes, preview, endPreview],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}
