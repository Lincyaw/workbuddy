import type { ComponentChildren } from 'preact';
import { useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { logout } from '../api/client';

interface NavItem {
  href: string;
  label: string;
}

const NAV: NavItem[] = [
  { href: '/', label: 'Dashboard' },
  { href: '/sessions', label: 'Sessions' },
];

function isActive(currentPath: string, href: string): boolean {
  if (href === '/') return currentPath === '/';
  return currentPath === href || currentPath.startsWith(`${href}/`);
}

export function Layout({ children }: { children: ComponentChildren }) {
  const { path } = useLocation();
  const [menuOpen, setMenuOpen] = useState(false);
  return (
    <div class="app">
      <header class={`topbar${menuOpen ? ' menu-open' : ''}`}>
        <a href="/" class="logo" aria-label="workbuddy home">
          workbuddy
        </a>
        <button
          type="button"
          class="topbar-toggle"
          aria-expanded={menuOpen}
          aria-controls="primary-nav"
          aria-label="Toggle navigation menu"
          onClick={() => setMenuOpen((v) => !v)}
        >
          <span aria-hidden="true">☰</span>
        </button>
        <nav id="primary-nav" aria-label="Primary">
          {NAV.map((item) => (
            <a
              href={item.href}
              class={isActive(path, item.href) ? 'active' : ''}
              aria-current={isActive(path, item.href) ? 'page' : undefined}
              onClick={() => setMenuOpen(false)}
            >
              {item.label}
            </a>
          ))}
        </nav>
        <button
          type="button"
          class="logout"
          onClick={() => {
            void logout();
          }}
        >
          Log out
        </button>
      </header>
      <main class="page">{children}</main>
    </div>
  );
}
