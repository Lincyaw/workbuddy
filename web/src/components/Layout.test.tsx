import { fireEvent, render } from '@testing-library/preact';
import { LocationProvider } from 'preact-iso';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { Layout } from './Layout';

const __dirname = dirname(fileURLToPath(import.meta.url));
const css = readFileSync(join(__dirname, '..', 'styles.css'), 'utf8');

function renderLayout() {
  return render(
    <LocationProvider>
      <Layout>
        <p>hello</p>
      </Layout>
    </LocationProvider>,
  );
}

describe('Layout mission-control shell', () => {
  beforeEach(() => {
    window.localStorage.clear();
    document.documentElement.removeAttribute('data-theme');
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, status: 200, text: async () => '[]' }));
  });

  it('renders a menu toggle and keeps the drawer closed by default', () => {
    const { container } = renderLayout();
    const toggle = container.querySelector('.wb-menu-toggle');
    expect(toggle).not.toBeNull();
    expect(container.querySelector('.wb-drawer.is-open')).toBeNull();
  });

  it('opens the mobile drawer when the menu button is clicked', () => {
    const { container } = renderLayout();
    const toggle = container.querySelector('.wb-menu-toggle') as HTMLButtonElement;
    fireEvent.click(toggle);
    expect(container.querySelector('.wb-drawer.is-open')).not.toBeNull();
  });

  it('cycles theme preference and persists to localStorage', () => {
    const { getByRole } = renderLayout();
    const themeButton = getByRole('button', { name: /Theme:/i });
    fireEvent.click(themeButton);
    expect(window.localStorage.getItem('wb:theme')).toBe('system');
    fireEvent.click(themeButton);
    expect(window.localStorage.getItem('wb:theme')).toBe('light');
    fireEvent.click(themeButton);
    expect(window.localStorage.getItem('wb:theme')).toBe('dark');
  });
});

describe('styles.css contract', () => {
  it('vendors the required fonts with font-display swap', () => {
    expect(css).toMatch(/font-family:\s*'Geist Sans'/);
    expect(css).toMatch(/font-family:\s*'JetBrains Mono'/);
    expect(css).toMatch(/font-display:\s*swap/);
  });

  it('includes the mobile breakpoint and ticker pause styles', () => {
    expect(css).toMatch(/@media \(max-width:\s*600px\)/);
    expect(css).toMatch(/\.wb-ticker\[data-paused='true'\] \.wb-ticker__track/);
  });

  it('documents the login shell recipe in the shared stylesheet', () => {
    expect(css).toMatch(/\.wb-login-shell/);
    expect(css).toMatch(/\.wb-login-card/);
    expect(css).toMatch(/\.wb-login-wordmark/);
  });
});
