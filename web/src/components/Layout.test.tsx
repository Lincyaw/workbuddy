import { fireEvent, render, screen } from '@testing-library/preact';
import { LocationProvider } from 'preact-iso';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { Layout } from './Layout';

vi.mock('../api/sessions', () => ({
  fetchSessions: vi.fn().mockResolvedValue([]),
}));

function renderLayout() {
  return render(
    <LocationProvider>
      <Layout>
        <p>hello</p>
      </Layout>
    </LocationProvider>,
  );
}

beforeEach(() => {
  window.localStorage.clear();
  document.documentElement.setAttribute('data-theme', 'dark');
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    value: vi.fn().mockImplementation(() => ({
      matches: false,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  });
});

describe('Layout shell', () => {
  it('renders a hamburger toggle and the primary nav links', () => {
    const { container } = renderLayout();
    const toggle = container.querySelector('.topbar-toggle');
    expect(toggle).not.toBeNull();
    expect(screen.getAllByText('Dashboard').length).toBeGreaterThan(0);
    expect(screen.getAllByText('Sessions').length).toBeGreaterThan(0);
    expect(screen.getAllByText('Hooks').length).toBeGreaterThan(0);
  });

  it('opens the mobile drawer when the hamburger is clicked', () => {
    const { container } = renderLayout();
    const toggle = container.querySelector('.topbar-toggle') as HTMLButtonElement;
    fireEvent.click(toggle);
    expect(container.querySelector('.topbar-drawer.open')).not.toBeNull();
  });

  it('cycles theme mode and persists it to localStorage', () => {
    renderLayout();
    const toggle = screen.getByLabelText(/Theme mode dark/i);
    fireEvent.click(toggle);
    expect(window.localStorage.getItem('wb:theme')).toBe('system');
    fireEvent.click(toggle);
    expect(window.localStorage.getItem('wb:theme')).toBe('light');
  });
});
