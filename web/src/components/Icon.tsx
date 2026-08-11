/*
 * The icon set.
 *
 * Inline SVG rather than an icon package: the panel is served by the daemon
 * itself under a strict CSP that blocks every external host, so a font or a
 * sprite from a CDN would silently render nothing. These are also the only
 * icons the panel uses, and twenty paths cost less than a dependency.
 *
 * Every icon draws with `currentColor` and inherits its size from the class,
 * so an icon inside a muted label is muted without anyone saying so.
 */

interface Props {
  /** Tailwind sizing/colour classes. Defaults to 16px, current colour. */
  className?: string;
}

function Svg({
  className = "h-4 w-4",
  children,
}: Props & { children: React.ReactNode }) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden="true"
    >
      {children}
    </svg>
  );
}

export function ServersIcon(props: Props) {
  return (
    <Svg {...props}>
      <rect x="3" y="4" width="18" height="7" rx="2" />
      <rect x="3" y="13" width="18" height="7" rx="2" />
      <path d="M7 7.5h.01M7 16.5h.01" />
    </Svg>
  );
}

export function SettingsIcon(props: Props) {
  return (
    <Svg {...props}>
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.7 1.7 0 0 0 .34 1.87l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.7 1.7 0 0 0-1.87-.34 1.7 1.7 0 0 0-1.03 1.56V21a2 2 0 1 1-4 0v-.09A1.7 1.7 0 0 0 8.9 19.3a1.7 1.7 0 0 0-1.87.34l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.7 1.7 0 0 0 4.7 15a1.7 1.7 0 0 0-1.56-1.03H3a2 2 0 1 1 0-4h.09A1.7 1.7 0 0 0 4.7 8.9a1.7 1.7 0 0 0-.34-1.87l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.7 1.7 0 0 0 9 4.6a1.7 1.7 0 0 0 1-1.56V3a2 2 0 1 1 4 0v.09a1.7 1.7 0 0 0 1.03 1.56 1.7 1.7 0 0 0 1.87-.34l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.7 1.7 0 0 0-.34 1.87V10a1.7 1.7 0 0 0 1.56 1h.01a2 2 0 1 1 0 4H21a1.7 1.7 0 0 0-1.6 1z" />
    </Svg>
  );
}

export function UsersIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M16 20v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2" />
      <circle cx="9" cy="7" r="4" />
      <path d="M22 20v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75" />
    </Svg>
  );
}

export function BotIcon(props: Props) {
  return (
    <Svg {...props}>
      <rect x="4" y="8" width="16" height="12" rx="2" />
      <path d="M12 8V4M9 14h.01M15 14h.01M2 13v2M22 13v2" />
    </Svg>
  );
}

export function PlayIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M7 4.5v15l12-7.5z" fill="currentColor" strokeWidth="1" />
    </Svg>
  );
}

export function StopIcon(props: Props) {
  return (
    <Svg {...props}>
      <rect
        x="6"
        y="6"
        width="12"
        height="12"
        rx="1.5"
        fill="currentColor"
        strokeWidth="1"
      />
    </Svg>
  );
}

export function RestartIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M21 12a9 9 0 1 1-2.64-6.36" />
      <path d="M21 3v6h-6" />
    </Svg>
  );
}

export function KillIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M13 2L4.5 13.5H11l-1 8.5 8.5-11.5H12z" />
    </Svg>
  );
}

export function PlayersIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M17 20v-2a4 4 0 0 0-4-4H7a4 4 0 0 0-4 4v2" />
      <circle cx="10" cy="7" r="3.5" />
    </Svg>
  );
}

export function CpuIcon(props: Props) {
  return (
    <Svg {...props}>
      <rect x="6" y="6" width="12" height="12" rx="2" />
      <path d="M10 2v4M14 2v4M10 18v4M14 18v4M2 10h4M2 14h4M18 10h4M18 14h4" />
    </Svg>
  );
}

export function MemoryIcon(props: Props) {
  return (
    <Svg {...props}>
      <rect x="3" y="7" width="18" height="10" rx="2" />
      <path d="M7 17v3M12 17v3M17 17v3M8 11h8" />
    </Svg>
  );
}

export function ClockIcon(props: Props) {
  return (
    <Svg {...props}>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 7v5l3 2" />
    </Svg>
  );
}

export function PlugIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M9 2v6M15 2v6M6 8h12v3a6 6 0 0 1-12 0z" />
      <path d="M12 17v5" />
    </Svg>
  );
}

export function FilesIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
    </Svg>
  );
}

export function SlidersIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M4 6h16M4 12h16M4 18h16" />
      <circle cx="9" cy="6" r="2" fill="currentColor" strokeWidth="1" />
      <circle cx="15" cy="12" r="2" fill="currentColor" strokeWidth="1" />
      <circle cx="8" cy="18" r="2" fill="currentColor" strokeWidth="1" />
    </Svg>
  );
}

export function PackageIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M21 8v8l-9 5-9-5V8l9-5z" />
      <path d="M3 8l9 5 9-5M12 13v8" />
    </Svg>
  );
}

export function ArchiveIcon(props: Props) {
  return (
    <Svg {...props}>
      <rect x="3" y="4" width="18" height="5" rx="1.5" />
      <path d="M5 9v9a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V9M10 13h4" />
    </Svg>
  );
}

export function TerminalIcon(props: Props) {
  return (
    <Svg {...props}>
      <rect x="3" y="4" width="18" height="16" rx="2" />
      <path d="M7 9l3 3-3 3M13 15h4" />
    </Svg>
  );
}

export function PlusIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M12 5v14M5 12h14" />
    </Svg>
  );
}

export function TrashIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M4 7h16M10 11v6M14 11v6" />
      <path d="M6 7l1 13a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-13M9 7V4h6v3" />
    </Svg>
  );
}

export function CopyIcon(props: Props) {
  return (
    <Svg {...props}>
      <rect x="9" y="9" width="12" height="12" rx="2" />
      <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
    </Svg>
  );
}

export function CheckIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M20 6L9 17l-5-5" />
    </Svg>
  );
}

export function SunIcon(props: Props) {
  return (
    <Svg {...props}>
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M2 12h2M20 12h2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
    </Svg>
  );
}

export function MoonIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" />
    </Svg>
  );
}

export function LogoutIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9" />
    </Svg>
  );
}

export function MenuIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M4 6h16M4 12h16M4 18h16" />
    </Svg>
  );
}

export function CloseIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M6 6l12 12M18 6L6 18" />
    </Svg>
  );
}

export function ChevronLeftIcon(props: Props) {
  return (
    <Svg {...props}>
      <path d="M15 6l-6 6 6 6" />
    </Svg>
  );
}

/** The mark. Four blocks, two of them accented — a nod, not a logo contest. */
export function Logo({ className = "h-5 w-5" }: Props) {
  return (
    <svg viewBox="0 0 20 20" className={className} aria-hidden="true">
      <rect x="1" y="1" width="8" height="8" rx="2" fill="var(--accent)" />
      <rect x="11" y="1" width="8" height="8" rx="2" fill="var(--text-faint)" />
      <rect x="1" y="11" width="8" height="8" rx="2" fill="var(--text-faint)" />
      <rect x="11" y="11" width="8" height="8" rx="2" fill="var(--accent)" />
    </svg>
  );
}
