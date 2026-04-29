import type { ComponentChildren } from 'preact';
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
  return (
    <div class="app">
      <header class="topbar">
        <a href="/" class="logo" aria-label="workbuddy home">
          workbuddy
        </a>
        <nav>
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
