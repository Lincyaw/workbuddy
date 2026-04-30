import { fireEvent, render, waitFor } from '@testing-library/preact';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { DispatchTicker, createDispatchTickerPoller } from './DispatchTicker';

const sample = [
  {
    id: 'session-1',
    timestamp: '2026-04-30T09:15:00Z',
    clock: '09:15:00',
    kind: 'DISPATCH',
    issue: '#287',
    agent: 'dev-agent',
    workerId: 'worker-1',
  },
];

describe('createDispatchTickerPoller', () => {
  it('debounces overlapping polling runs', async () => {
    let resolveLoad: (value: typeof sample) => void = () => {};
    const load = vi.fn(
      () =>
        new Promise<typeof sample>((resolve) => {
          resolveLoad = resolve;
        }),
    );
    const apply = vi.fn();
    const poll = createDispatchTickerPoller(load, apply);

    const first = poll();
    const second = poll();

    expect(load).toHaveBeenCalledTimes(1);
    resolveLoad(sample);
    await first;
    await second;
    expect(apply).toHaveBeenCalledWith(sample);
  });
});

describe('DispatchTicker pause states', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('pauses on hover and resumes on escape', async () => {
    const loadEntries = vi.fn().mockResolvedValue(sample);
    const view = render(<DispatchTicker loadEntries={loadEntries} intervalMs={5_000} />);

    await waitFor(() => expect(view.getByTestId('ticker-track-wrap').getAttribute('data-paused')).toBeNull());
    const ticker = view.container.querySelector('.wb-ticker') as HTMLElement;
    fireEvent.mouseEnter(ticker);
    expect(ticker.dataset.paused).toBe('true');
    fireEvent.keyDown(ticker, { key: 'Escape' });
    expect(ticker.dataset.paused).toBe('false');
  });
});
