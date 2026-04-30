import { fireEvent, render } from '@testing-library/preact';
import { LocationProvider } from 'preact-iso';
import { describe, expect, it, vi } from 'vitest';
import type { ComponentChild } from 'preact';
import type { HookListEntry } from '../api/hooks';
import { HookList } from './HookList';

function makeEntry(over: Partial<HookListEntry> = {}): HookListEntry {
  return {
    name: 'h1',
    events: ['alert'],
    action_type: 'webhook',
    enabled: true,
    auto_disabled: false,
    successes: 4,
    failures: 1,
    filtered: 0,
    disabled_drops: 0,
    overflow: 0,
    consecutive_failures: 0,
    duration_count: 5,
    duration_sum_ns: 0,
    ...over,
  };
}

function renderWithRouter(ui: ComponentChild) {
  return render(<LocationProvider>{ui}</LocationProvider>);
}

describe('HookList', () => {
  it('renders one desktop row per hook with name and action', () => {
    const { container } = renderWithRouter(<HookList hooks={[makeEntry({ name: 'alpha' }), makeEntry({ name: 'beta' })]} onSelect={() => {}} />);
    const rows = container.querySelectorAll('tbody tr');
    expect(rows.length).toBe(2);
    expect(rows[0].textContent).toContain('alpha');
    expect(rows[0].textContent).toContain('webhook');
  });

  it('computes the error rate from successes plus failures', () => {
    const { container } = renderWithRouter(<HookList hooks={[makeEntry({ successes: 3, failures: 1 })]} onSelect={() => {}} />);
    expect(container.querySelector('tbody tr td:nth-child(6)')?.textContent).toBe('25%');
  });

  it('routes when a row is clicked', () => {
    const onSelect = vi.fn();
    const { container } = renderWithRouter(<HookList hooks={[makeEntry({ name: 'alpha' })]} onSelect={onSelect} />);
    fireEvent.click(container.querySelector('tbody tr') as HTMLElement);
    expect(onSelect).toHaveBeenCalledWith('alpha');
  });

  it('renders 0% error rate cleanly when there are no calls yet', () => {
    const { container } = renderWithRouter(<HookList hooks={[makeEntry({ successes: 0, failures: 0 })]} onSelect={() => {}} />);
    expect(container.querySelector('tbody tr td:nth-child(6)')?.textContent).toBe('0%');
  });
});
