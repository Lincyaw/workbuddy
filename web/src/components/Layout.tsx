import type { ComponentChildren } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { logout } from '../api/client';
import {
  applyThemePreference,
  cycleThemePreference,
  persistThemePreference,
  readThemePreference,
  type ThemePreference,
} from '../theme';

interface NavItem {
  href: string;
  label: string;
}

const NAV: NavItem[] = [
  { href: '/', label: 'Dashboard' },
  { href: '/sessions', label: 'Sessions' },
  { href: '/hooks', label: 'Hooks' },
];

function isActive(currentPath: string, href: string): boolean {
  if (href === '/') return currentPath === '/';
  return currentPath === href || currentPath.startsWith(`${href}/`);
}

function themeIcon(theme: ThemePreference): string {
  if (theme === 'light') return 'Sun';
  if (theme === 'dark') return 'Moon';
  return 'Auto';
}

export function Layout({ children }: { children: ComponentChildren }) {
  const { path } = useLocation();
  const [menuOpen, setMenuOpen] = useState(false);
  const [themePreference, setThemePreference] = useState<ThemePreference>(() => readThemePreference());

  useEffect(() => {
    applyThemePreference(themePreference);
    persistThemePreference(themePreference);
  }, [themePreference]);

  useEffect(() => {
    if (themePreference !== 'system' || typeof window === 'undefined' || !window.matchMedia) return;
    const media = window.matchMedia('(prefers-color-scheme: dark)');
    const sync = () => applyThemePreference('system');
    sync();
    media.addEventListener?.('change', sync);
    return () => media.removeEventListener?.('change', sync);
  }, [themePreference]);

  useEffect(() => {
    setMenuOpen(false);
  }, [path]);

  const themeLabel = useMemo(() => `Theme: ${themePreference}`, [themePreference]);

  function advanceTheme() {
    setThemePreference((current) => cycleThemePreference(current));
  }

  function handleThemeKeyDown(e: KeyboardEvent) {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      advanceTheme();
    }
  }

  return (
    <div class="app">
      <header class={`wb-topbar${menuOpen ? ' is-open' : ''}`}>
        <div class="wb-topbar__row">
          <div class="wb-topbar__brand-group">
            <a href="/" class="wb-topbar__logo" aria-label="workbuddy home">
              <span class="wb-topbar__wordmark">Workbuddy</span>
              <span class="wb-topbar__brand-note">Operator console</span>
            </a>
            <div class="wb-topbar__identity" aria-label="Repository and environment">
              <span class="wb-topbar__chip">repo: Lincyaw/workbuddy</span>
              <span class="wb-topbar__chip">env: coordinator ui</span>
            </div>
          </div>

          <div class="wb-topbar__actions">
            <button
              type="button"
              class="wb-icon-button wb-topbar__menu-toggle"
              aria-expanded={menuOpen}
              aria-controls="primary-nav"
              aria-label={menuOpen ? 'Close navigation menu' : 'Open navigation menu'}
              onClick={() => setMenuOpen((value) => !value)}
            >
              <span aria-hidden="true">{menuOpen ? 'X' : 'Menu'}</span>
            </button>
            <button
              type="button"
              role="button"
              class="wb-icon-button"
              aria-label={themeLabel}
              title={`${themeLabel} - activate to cycle system, light, dark`}
              onClick={advanceTheme}
              onKeyDown={handleThemeKeyDown}
            >
              <span aria-hidden="true">{themeIcon(themePreference)}</span>
            </button>
            <button
              type="button"
              class="wb-button wb-button--ghost"
              onClick={() => {
                void logout();
              }}
            >
              Log out
            </button>
          </div>
        </div>

        <div class="wb-topbar__drawer">
          <nav id="primary-nav" class="wb-topbar__nav" aria-label="Primary">
            {NAV.map((item) => (
              <a
                href={item.href}
                class={isActive(path, item.href) ? 'is-active' : ''}
                aria-current={isActive(path, item.href) ? 'page' : undefined}
              >
                {item.label}
              </a>
            ))}
          </nav>
        </div>
      </header>
      <main class="page">{children}</main>
    </div>
  );
}
