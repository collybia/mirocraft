import { Link, NavLink, useNavigate } from "react-router-dom";
import type { ReactNode } from "react";

import * as api from "../lib/api";
import { useTheme } from "../ThemeProvider";

interface Props {
  user: api.Me;
  onLoggedOut: () => void;
  children: ReactNode;
}

export function Layout({ user, onLoggedOut, children }: Props) {
  const navigate = useNavigate();
  const { base, toggleLightDark } = useTheme();

  async function handleLogout() {
    await api.logout();
    onLoggedOut();
    navigate("/login");
  }

  return (
    <div className="min-h-screen bg-surface text-body">
      <header className="border-b border-line bg-elevated">
        <div className="mx-auto flex max-w-6xl items-center gap-4 px-4 py-3">
          <Link to="/" className="flex items-center gap-2 font-semibold">
            <Logo />
            Mirocraft
          </Link>

          <nav className="ml-4 flex items-center gap-1 text-sm">
            <NavItem to="/">Серверы</NavItem>
            <NavItem to="/settings">Настройки</NavItem>
            {user.role === "admin" && <NavItem to="/admin">Пользователи</NavItem>}
            {user.role === "admin" && <NavItem to="/bots">Боты</NavItem>}
          </nav>

          <div className="ml-auto flex items-center gap-2">
            <button
              type="button"
              onClick={() => void toggleLightDark()}
              className="btn btn-ghost px-2 py-1"
              title={base === "dark" ? "Переключить на светлую" : "Переключить на тёмную"}
              aria-label="Переключить тему"
            >
              {base === "dark" ? <SunIcon /> : <MoonIcon />}
            </button>

            <span className="hidden text-sm text-muted sm:inline">{user.email}</span>

            <button type="button" onClick={() => void handleLogout()} className="btn btn-ghost text-sm">
              Выйти
            </button>
          </div>
        </div>
      </header>

      <main className="mx-auto max-w-6xl px-4 py-6">{children}</main>

      {/*
        Not decoration and not a credit line: the AGPL asks that people who
        use the program over a network be offered its source, and this panel
        is exactly that. One line, at the bottom, where it costs nobody
        anything and is there when someone looks for it.
      */}
      <footer className="mx-auto max-w-6xl px-4 pb-6 text-xs text-faint">
        Mirocraft · AGPL-3.0 ·{" "}
        <a
          href="https://github.com/collybia/mirocraft"
          target="_blank"
          rel="noreferrer noopener"
          className="hover:text-accent"
        >
          исходный код
        </a>
      </footer>
    </div>
  );
}

function NavItem({ to, children }: { to: string; children: ReactNode }) {
  return (
    <NavLink
      to={to}
      end={to === "/"}
      className={({ isActive }) =>
        [
          "rounded-sm px-3 py-1.5",
          isActive ? "bg-surface text-body" : "text-muted hover:text-body",
        ].join(" ")
      }
    >
      {children}
    </NavLink>
  );
}

function Logo() {
  return (
    <svg width="20" height="20" viewBox="0 0 20 20" aria-hidden="true">
      <rect x="1" y="1" width="8" height="8" rx="1.5" fill="var(--accent)" />
      <rect x="11" y="1" width="8" height="8" rx="1.5" fill="var(--text-faint)" />
      <rect x="1" y="11" width="8" height="8" rx="1.5" fill="var(--text-faint)" />
      <rect x="11" y="11" width="8" height="8" rx="1.5" fill="var(--accent)" />
    </svg>
  );
}

function SunIcon() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true">
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M2 12h2M20 12h2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
    </svg>
  );
}

function MoonIcon() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true">
      <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" />
    </svg>
  );
}
