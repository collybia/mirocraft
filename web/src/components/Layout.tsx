import { useEffect, useState, type ReactNode } from "react";
import { Link, NavLink, useLocation, useNavigate } from "react-router-dom";

import * as api from "../lib/api";
import { useTheme } from "../ThemeProvider";
import {
  BotIcon,
  CloseIcon,
  Logo,
  LogoutIcon,
  MenuIcon,
  MoonIcon,
  ServersIcon,
  SettingsIcon,
  SunIcon,
  UsersIcon,
} from "./Icon";

interface Props {
  user: api.Me;
  onLoggedOut: () => void;
  children: ReactNode;
}

/**
 * The application shell: a permanent sidebar beside the page.
 *
 * The panel used to navigate through text links in a top bar, which put the
 * whole navigation on one line and left the content floating in the middle of
 * a wide screen with nothing around it. A sidebar gives the page an edge to
 * sit against, keeps every destination visible at once, and leaves the top of
 * the content area for what the page is actually about.
 */
export function Layout({ user, onLoggedOut, children }: Props) {
  const navigate = useNavigate();
  const location = useLocation();
  const { base, toggleLightDark } = useTheme();
  const [menuOpen, setMenuOpen] = useState(false);

  // Navigating on a phone should close the drawer. Without this the new page
  // renders behind a menu that is still covering it.
  useEffect(() => setMenuOpen(false), [location.pathname]);

  async function handleLogout() {
    await api.logout();
    onLoggedOut();
    navigate("/login");
  }

  const items = [
    { to: "/", label: "Серверы", icon: <ServersIcon /> },
    { to: "/settings", label: "Настройки", icon: <SettingsIcon /> },
    ...(user.role === "admin"
      ? [
          { to: "/admin", label: "Пользователи", icon: <UsersIcon /> },
          { to: "/bots", label: "Боты", icon: <BotIcon /> },
        ]
      : []),
  ];

  return (
    <div className="min-h-screen bg-surface text-body">
      {/* The bar exists only below the sidebar's breakpoint. */}
      <header className="sticky top-0 z-30 flex items-center gap-3 border-b border-line bg-sunken px-4 py-2.5 lg:hidden">
        <button
          type="button"
          className="btn btn-quiet btn-icon"
          onClick={() => setMenuOpen((open) => !open)}
          aria-label={menuOpen ? "Закрыть меню" : "Открыть меню"}
          aria-expanded={menuOpen}
        >
          {menuOpen ? <CloseIcon /> : <MenuIcon />}
        </button>
        <Link to="/" className="flex items-center gap-2 font-semibold">
          <Logo />
          Mirocraft
        </Link>
      </header>

      {menuOpen && (
        <button
          type="button"
          className="fixed inset-0 z-30 bg-inset opacity-70 lg:hidden"
          onClick={() => setMenuOpen(false)}
          aria-label="Закрыть меню"
          tabIndex={-1}
        />
      )}

      <div className="lg:flex">
        <aside
          className={[
            "fixed inset-y-0 left-0 z-40 flex w-sidebar flex-col border-r border-line bg-sunken",
            "transition-transform lg:sticky lg:top-0 lg:h-screen lg:translate-x-0",
            menuOpen ? "translate-x-0" : "-translate-x-full",
          ].join(" ")}
        >
          <div className="flex items-center gap-2 px-4 py-4">
            <Link
              to="/"
              className="flex items-center gap-2 text-base font-semibold"
            >
              <Logo className="h-6 w-6" />
              Mirocraft
            </Link>
            <button
              type="button"
              className="btn btn-quiet btn-icon ml-auto lg:hidden"
              onClick={() => setMenuOpen(false)}
              aria-label="Закрыть меню"
            >
              <CloseIcon />
            </button>
          </div>

          <nav className="flex flex-1 flex-col gap-0.5 px-3">
            {items.map((item) => (
              <NavLink
                key={item.to}
                to={item.to}
                end={item.to === "/"}
                className={({ isActive }) =>
                  isActive ? "nav-item nav-item-active" : "nav-item"
                }
              >
                {item.icon}
                {item.label}
              </NavLink>
            ))}
          </nav>

          <div className="border-t border-line p-3">
            <div className="mb-2 flex items-center gap-2 px-1">
              <Avatar email={user.email} />
              <div className="min-w-0">
                <div className="truncate text-sm">{user.email}</div>
                <div className="text-xs text-faint">
                  {user.role === "admin" ? "администратор" : "пользователь"}
                </div>
              </div>
            </div>

            <div className="flex gap-1">
              <button
                type="button"
                onClick={() => void toggleLightDark()}
                className="btn btn-quiet btn-sm flex-1"
                title={
                  base === "dark"
                    ? "Переключить на светлую"
                    : "Переключить на тёмную"
                }
                aria-label="Переключить тему"
              >
                {base === "dark" ? <SunIcon /> : <MoonIcon />}
                {base === "dark" ? "Светлая" : "Тёмная"}
              </button>
              <button
                type="button"
                onClick={() => void handleLogout()}
                className="btn btn-quiet btn-sm"
                title="Выйти"
              >
                <LogoutIcon />
                Выйти
              </button>
            </div>
          </div>
        </aside>

        <div className="min-w-0 flex-1">
          <main className="mx-auto max-w-6xl px-4 py-6 sm:px-6 lg:py-8">
            {children}
          </main>

          {/*
            Not decoration and not a credit line: the AGPL asks that people who
            use the program over a network be offered its source, and this
            panel is exactly that. One line, at the bottom, where it costs
            nobody anything and is there when someone looks for it.
          */}
          <footer className="mx-auto max-w-6xl px-4 pb-8 text-xs text-faint sm:px-6">
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
      </div>
    </div>
  );
}

/** The first letter of the account name, so the sidebar foot is not all text. */
function Avatar({ email }: { email: string }) {
  return (
    <span className="flex h-8 w-8 flex-none items-center justify-center rounded-full bg-accent-bg text-sm font-medium text-accent">
      {email.slice(0, 1).toUpperCase()}
    </span>
  );
}

/**
 * PageHeader is the top of every page: what this is, and what you can do to it.
 *
 * Shared rather than repeated so that the title, the description and the
 * actions keep the same size and spacing everywhere — the thing that made the
 * old pages feel assembled by different people.
 */
export function PageHeader({
  title,
  description,
  actions,
  back,
}: {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  back?: ReactNode;
}) {
  return (
    <div className="mb-6">
      {back}
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
          {description && (
            <p className="mt-1 text-sm text-muted">{description}</p>
          )}
        </div>
        {actions && (
          <div className="flex flex-wrap items-center gap-2">{actions}</div>
        )}
      </div>
    </div>
  );
}
