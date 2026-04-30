import { fireEvent, render } from '@testing-library/preact';
import { LocationProvider } from 'preact-iso';
import { describe, expect, it } from 'vitest';
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
  it('renders a hamburger toggle that collapses the nav by default', () => {
    const { container } = renderLayout();
    const toggle = container.querySelector('.topbar-toggle');
    expect(toggle).not.toBeNull();
    expect(toggle?.getAttribute('aria-expanded')).toBe('false');
    expect(container.querySelector('.topbar.menu-open')).toBeNull();
  });

  it('toggles menu-open class when the hamburger is clicked', () => {
    const { container } = renderLayout();
    const toggle = container.querySelector('.topbar-toggle') as HTMLButtonElement;
    fireEvent.click(toggle);
    expect(container.querySelector('.topbar.menu-open')).not.toBeNull();
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
});

describe('styles.css responsive contract', () => {
  it('declares both ≤480 and ≤768 breakpoints', () => {
    expect(css).toMatch(/@media\s*\(max-width:\s*480px\)/);
    expect(css).toMatch(/@media\s*\(max-width:\s*768px\)/);
  });

  it('makes form inputs at least 16px on narrow screens (iOS no-zoom)', () => {
    // Pull the ≤768px block and assert the input rule sets font-size:16px.
    const block = css.match(/@media\s*\(max-width:\s*768px\)\s*{([\s\S]*?)\n}\n/);
    expect(block).not.toBeNull();
    expect(block?.[1]).toMatch(/input\[type="password"\][\s\S]*font-size:\s*16px/);
  });

  it('pins the first table column on narrow screens', () => {
    const block = css.match(/@media\s*\(max-width:\s*768px\)\s*{([\s\S]*?)\n}\n/);
    expect(block?.[1]).toMatch(/th:first-child[\s\S]*position:\s*sticky/);
  });

  it('collapses session metadata to a single column on narrow screens', () => {
    const block = css.match(/@media\s*\(max-width:\s*768px\)\s*{([\s\S]*?)\n}\n/);
    expect(block?.[1]).toMatch(/\.wb-meta-grid[\s\S]*grid-template-columns:\s*1fr/);
  });

  it('thins the pretty-mode turn boundary on narrow screens', () => {
    const block = css.match(/@media\s*\(max-width:\s*768px\)\s*{([\s\S]*?)\n}\n/);
    expect(block?.[1]).toMatch(/\.wb-turn-group[\s\S]*border-left-width:\s*1px/);
  });
});
