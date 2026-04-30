import { fireEvent, render } from '@testing-library/preact';
import { LocationProvider } from 'preact-iso';
import { describe, expect, it, beforeEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { Layout } from './Layout';

const __dirname = dirname(fileURLToPath(import.meta.url));
const stylesPath = join(__dirname, '..', 'styles.css');
const css = readFileSync(stylesPath, 'utf8');

function renderLayout() {
  return render(
    <LocationProvider>
      <Layout>
        <p>hello</p>
      </Layout>
    </LocationProvider>,
  );
}

describe('Layout responsive shell', () => {
  beforeEach(() => {
    window.localStorage.clear();
    document.documentElement.removeAttribute('data-theme');
  });

  it('renders a hamburger toggle that keeps the drawer closed by default', () => {
    const { container } = renderLayout();
    const toggle = container.querySelector('.wb-topbar__menu-toggle');
    expect(toggle).not.toBeNull();
    expect(toggle?.getAttribute('aria-expanded')).toBe('false');
    expect(container.querySelector('.wb-topbar.is-open')).toBeNull();
  });

  it('toggles the topbar drawer when the menu button is clicked', () => {
    const { container } = renderLayout();
    const toggle = container.querySelector('.wb-topbar__menu-toggle') as HTMLButtonElement;
    fireEvent.click(toggle);
    expect(container.querySelector('.wb-topbar.is-open')).not.toBeNull();
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
  });

  it('keeps a primary nav with all known links', () => {
    const { container } = renderLayout();
    const nav = container.querySelector('nav#primary-nav');
    expect(nav).not.toBeNull();
    const labels = Array.from(nav?.querySelectorAll('a') ?? []).map((a) => a.textContent);
    expect(labels).toContain('Dashboard');
    expect(labels).toContain('Sessions');
    expect(labels).toContain('Hooks');
  });

  it('cycles theme preference and persists it to localStorage', () => {
    const { getByRole } = renderLayout();
    const themeButton = getByRole('button', { name: /Theme:/i });
    fireEvent.click(themeButton);
    expect(window.localStorage.getItem('wb:theme')).toBe('light');
    expect(document.documentElement.dataset.theme).toBe('light');
    fireEvent.click(themeButton);
    expect(window.localStorage.getItem('wb:theme')).toBe('dark');
    expect(document.documentElement.dataset.theme).toBe('dark');
  });
});

describe('styles.css responsive contract', () => {
  it('declares the 900px and 600px breakpoints used by the refreshed shell', () => {
    expect(css).toMatch(/@media\s*\(max-width:\s*900px\)/);
    expect(css).toMatch(/@media\s*\(max-width:\s*600px\)/);
  });

  it('keeps form controls at 16px on narrow screens', () => {
    const block = css.match(/@media\s*\(max-width:\s*600px\)\s*{([\s\S]*?)\n}\n/);
    expect(block).not.toBeNull();
    expect(block?.[1]).toMatch(/input,[\s\S]*font-size:\s*16px/);
  });

  it('pins the first table column on narrow screens', () => {
    const block = css.match(/@media\s*\(max-width:\s*600px\)\s*{([\s\S]*?)\n}\n/);
    expect(block?.[1]).toMatch(/th:first-child[\s\S]*position:\s*sticky/);
  });

  it('collapses session metadata to a single column on narrow screens', () => {
    const block = css.match(/@media\s*\(max-width:\s*600px\)\s*{([\s\S]*?)\n}\n/);
    expect(block?.[1]).toMatch(/\.wb-meta-grid[\s\S]*grid-template-columns:\s*1fr/);
  });

  it('keeps the turn boundary rail thin on narrow screens', () => {
    const block = css.match(/@media\s*\(max-width:\s*600px\)\s*{([\s\S]*?)\n}\n/);
    expect(block?.[1]).toMatch(/\.wb-turn-group[\s\S]*padding-left:\s*var\(--sp-2\)/);
  });
});
