import { fireEvent, render } from '@testing-library/preact';
import { describe, expect, it, vi } from 'vitest';
import { HookList } from './HookList';
import type { HookListEntry } from '../api/hooks';

function makeEntry(overrides: Partial<HookListEntry> = {}): HookListEntry {
  return {
    name: 'hook-alpha',
    events: ['dispatch'],
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
    ...overrides,
  };
}

describe('HookList', () => {
  it('renders one card per hook with state and error rate', () => {
    const { getByText, getAllByText } = render(
      <HookList
        hooks={[makeEntry({ name: 'alpha' }), makeEntry({ name: 'beta', auto_disabled: true })]}
        latencySamples={{ alpha: [1, 2, 3], beta: [2, 4, 8] }}
        onSelect={() => {}}
      />,
    );
    expect(getByText('alpha')).toBeTruthy();
    expect(getByText('beta')).toBeTruthy();
    expect(getAllByText('20% error')).toHaveLength(2);
    expect(getByText('auto-disabled')).toBeTruthy();
  });

  it('routes when a card is clicked', () => {
    const onSelect = vi.fn();
    const { getByText } = render(
      <HookList hooks={[makeEntry({ name: 'alpha' })]} latencySamples={{}} onSelect={onSelect} />,
    );
    fireEvent.click(getByText('alpha').closest('button') as HTMLButtonElement);
    expect(onSelect).toHaveBeenCalledWith('alpha');
  });
});
