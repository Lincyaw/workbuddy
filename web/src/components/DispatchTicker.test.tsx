import { fireEvent, render, waitFor } from '@testing-library/preact';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { DispatchTicker, TICKER_POLL_MS } from './DispatchTicker';
import { fetchSessions } from '../api/sessions';

vi.mock('../api/sessions', () => ({
  fetchSessions: vi.fn(),
}));

function makeSession(overrides: Record<string, unknown> = {}) {
  return {
    session_id: 'session-1',
    repo: 'Lincyaw/workbuddy',
    issue_num: 291,
    agent_name: 'codex',
    attempt: 1,
    status: 'running',
    exit_code: 0,
    duration: 32,
    created_at: '2026-04-30T10:00:00Z',
    worker_id: 'worker-alpha',
    runtime: 'codex',
    ...overrides,
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

describe('DispatchTicker', () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.clearAllMocks();
  });

  // Phase 3 (REQ-122): fetchSessions now returns
  // {rows, offlineWorkers}. Tests build the result via this helper.
  function ok(rows: ReturnType<typeof makeSession>[], offlineWorkers: string[] = []) {
    return { rows, offlineWorkers };
  }

  it('debounces polling while a previous request is still in flight', async () => {
    vi.useFakeTimers();
    const pending = deferred<ReturnType<typeof ok>>();
    vi.mocked(fetchSessions).mockReturnValue(pending.promise as never);

    const { container } = render(<DispatchTicker />);
    expect(fetchSessions).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(TICKER_POLL_MS * 2);
    expect(fetchSessions).toHaveBeenCalledTimes(1);

    pending.resolve(ok([makeSession()]));
    await waitFor(() => expect(container.textContent).toContain('DISPATCHED'));
  });

  it('pauses the marquee while hovered', async () => {
    vi.mocked(fetchSessions).mockResolvedValue(ok([makeSession()]) as never);
    const { container } = render(<DispatchTicker />);
    await waitFor(() => expect(container.textContent).toContain('DISPATCHED'));
    const ticker = container.querySelector('.dispatch-ticker') as HTMLElement;
    fireEvent.mouseEnter(ticker);
    expect(ticker.className).toContain('is-paused');
    fireEvent.mouseLeave(ticker);
    expect(ticker.className).not.toContain('is-paused');
  });

  it('dims after an error and keeps the last successful batch visible', async () => {
    vi.useFakeTimers();
    vi.mocked(fetchSessions)
      .mockResolvedValueOnce(ok([makeSession()]) as never)
      .mockRejectedValueOnce(new Error('boom') as never);

    const { container } = render(<DispatchTicker />);
    await waitFor(() => expect(container.textContent).toContain('DISPATCHED'));

    await vi.advanceTimersByTimeAsync(TICKER_POLL_MS + 1);
    await waitFor(() => expect(container.querySelector('.dispatch-ticker--degraded')).not.toBeNull());
    expect(container.textContent).toContain('DISPATCHED');
  });
});
