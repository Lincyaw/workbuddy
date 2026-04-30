import type { ComponentChildren } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { logout } from '../api/client';
import { DispatchTicker } from './DispatchTicker';
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
  if (href === '/') return currentPath === '/' || currentPath === '/dashboard';
  return currentPath === href || currentPath.startsWith(`${href}/`);
}

function themeLabel(theme: ThemePreference): string {
  if (theme === 'system') return 'Theme: system';
  if (theme === 'light') return 'Theme: light';
  return 'Theme: dark';
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

  const label = useMemo(() => themeLabel(themePreference), [themePreference]);

  return (
    <div class="wb-shell">
      <header class="wb-topbar">
        <div class="mx-auto flex h-[52px] w-full max-w-[1280px] items-center justify-between gap-4 px-4 md:px-6">
          <div class="flex min-w-0 items-center gap-5">
            <a href="/" class="wb-wordmark" aria-label="workbuddy home">
              WORKBUDDY
            </a>
            <nav class="hidden items-center gap-5 text-[14px] md:flex" aria-label="Primary navigation">
              {NAV.map((item) => {
                const active = isActive(path, item.href);
                return (
                  <a href={item.href} class={`wb-nav-link${active ? ' is-active' : ''}`} aria-current={active ? 'page' : undefined}>
                    {item.label}
                  </a>
                );
              })}
            </nav>
          </div>

          <div class="flex items-center gap-2">
            <button
              type="button"
              class="wb-theme-toggle"
              aria-label={label}
              title={`${label}; activate to cycle system, light, dark`}
              onClick={() => setThemePreference((current) => cycleThemePreference(current))}
            >
              <span class="font-mono text-[11px] uppercase tracking-[0.18em]">{themePreference}</span>
            </button>
            <button type="button" class="wb-cta wb-cta--ghost hidden sm:inline-flex" onClick={() => void logout()}>
              logout
            </button>
            <button
              type="button"
              class="wb-menu-toggle md:hidden"
              aria-expanded={menuOpen}
              aria-controls="primary-drawer"
              onClick={() => setMenuOpen((value) => !value)}
            >
              {menuOpen ? 'close' : 'menu'}
            </button>
          </div>
        </div>
        <div id="primary-drawer" class={`wb-drawer md:hidden${menuOpen ? ' is-open' : ''}`}>
          <nav class="mx-auto flex w-full max-w-[1280px] flex-col gap-1 px-4 pb-4" aria-label="Mobile navigation">
            {NAV.map((item) => {
              const active = isActive(path, item.href);
              return (
                <a href={item.href} class={`wb-drawer-link${active ? ' is-active' : ''}`} aria-current={active ? 'page' : undefined}>
                  {item.label}
                </a>
              );
            })}
            <button type="button" class="wb-cta wb-cta--ghost mt-2 justify-center sm:hidden" onClick={() => void logout()}>
              logout
            </button>
          </nav>
        </div>
      </header>
      <DispatchTicker />
      <main class="mx-auto w-full max-w-[1280px] px-4 pb-10 pt-6 md:px-6">{children}</main>
    </div>
  );
}
