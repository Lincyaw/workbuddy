import type { ComponentChildren } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { logout } from '../api/client';
import { DispatchTicker } from './DispatchTicker';
import {
  applyTheme,
  getStoredTheme,
  getSystemTheme,
  THEME_STORAGE_KEY,
  type ThemeMode,
} from '../theme';

interface NavItem {
  href: string;
  label: string;
}

const NAV: NavItem[] = [
  { href: '/dashboard', label: 'Dashboard' },
  { href: '/sessions', label: 'Sessions' },
  { href: '/hooks', label: 'Hooks' },
];

function isActive(currentPath: string, href: string): boolean {
  if (href === '/dashboard') {
    return currentPath === '/' || currentPath === '/dashboard';
  }
  return currentPath === href || currentPath.startsWith(`${href}/`);
}

function nextTheme(mode: ThemeMode): ThemeMode {
  switch (mode) {
    case 'system':
      return 'light';
    case 'light':
      return 'dark';
    default:
      return 'system';
  }
}

export function Layout({ children }: { children: ComponentChildren }) {
  const { path } = useLocation();
  const [menuOpen, setMenuOpen] = useState(false);
  const [themeMode, setThemeMode] = useState<ThemeMode>(() => getStoredTheme());
  const resolvedTheme = useMemo(
    () => (themeMode === 'system' ? getSystemTheme() : themeMode),
    [themeMode],
  );

  useEffect(() => {
    applyTheme(themeMode);
    window.localStorage.setItem(THEME_STORAGE_KEY, themeMode);
    if (themeMode !== 'system') return;
    const media = window.matchMedia('(prefers-color-scheme: light)');
    const sync = () => applyTheme('system');
    media.addEventListener('change', sync);
    return () => media.removeEventListener('change', sync);
  }, [themeMode]);

  useEffect(() => {
    setMenuOpen(false);
  }, [path]);

  return (
    <div class="app-shell">
      <header class={`topbar${menuOpen ? ' menu-open' : ''}`}>
        <div class="topbar-inner">
          <a href="/dashboard" class="logo" aria-label="workbuddy home">
            WORKBUDDY
          </a>

          <button
            type="button"
            class="topbar-toggle"
            aria-expanded={menuOpen}
            aria-controls="primary-nav"
            aria-label="Toggle navigation menu"
            onClick={() => setMenuOpen((value) => !value)}
          >
            <span />
            <span />
            <span />
          </button>

          <nav id="primary-nav" aria-label="Primary" class="topbar-nav">
            {NAV.map((item) => (
              <a
                href={item.href}
                class={isActive(path, item.href) ? 'active' : ''}
                aria-current={isActive(path, item.href) ? 'page' : undefined}
              >
                {item.label}
              </a>
            ))}
          </nav>

          <div class="topbar-actions">
            <button
              type="button"
              class="theme-toggle"
              onClick={() => setThemeMode((value) => nextTheme(value))}
              aria-label={`Theme mode ${themeMode}; click to switch`}
            >
              <span class="theme-toggle-label">theme</span>
              <strong>{themeMode}</strong>
              <span class="theme-toggle-state">{resolvedTheme}</span>
            </button>
            <button
              type="button"
              class="logout"
              onClick={() => {
                void logout();
              }}
            >
              Log out
            </button>
          </div>
        </div>

        <div class={`topbar-drawer${menuOpen ? ' open' : ''}`}>
          {NAV.map((item) => (
            <a
              href={item.href}
              class={isActive(path, item.href) ? 'active' : ''}
              aria-current={isActive(path, item.href) ? 'page' : undefined}
            >
              {item.label}
            </a>
          ))}
        </div>
      </header>
      <DispatchTicker />
      <main class="page">{children}</main>
    </div>
  );
}
