import { describe, expect, it } from 'vitest';
import type { SessionEvent } from '../api/sessions';
import { mergeSessionEvents } from './sessionEvents';

function ev(index: number, kind = 'log'): SessionEvent {
  return { index, kind, seq: index };
}

describe('mergeSessionEvents', () => {
  it('SSE arrives before fetch resolves: fetch must merge, not clobber', () => {
    // The slow-network race from issue #277. Order of operations:
    //  1. The detail page mounts; both initial fetch and SSE start.
    //  2. SSE replays events 0,1,2 first (fetch is still pending).
    //  3. Initial fetch resolves with events 0..4 (replay + the next two).
    // Before the fix this clobbered the SSE-already-rendered events because
    // the fetch handler called setEvents(next) with a freshly-built array
    // whose dedupe filter had already eaten 0/1/2.
    const seen = new Set<number>();

    // Step 2: SSE pushes 0,1,2 — one event at a time, the way the live
    // handler sees it.
    let state: SessionEvent[] = [];
    state = mergeSessionEvents(state, [ev(0)], seen);
    state = mergeSessionEvents(state, [ev(1)], seen);
    state = mergeSessionEvents(state, [ev(2)], seen);
    expect(state.map((e) => e.index)).toEqual([0, 1, 2]);

    // Step 3: fetch resolves with the trailing 0..4. The whole batch goes
    // through mergeSessionEvents the same way it does inside setEvents.
    state = mergeSessionEvents(state, [ev(0), ev(1), ev(2), ev(3), ev(4)], seen);

    expect(state).toHaveLength(5);
    expect(state.map((e) => e.index)).toEqual([0, 1, 2, 3, 4]);
    // Indices are strictly monotonic regardless of arrival order.
    for (let i = 1; i < state.length; i += 1) {
      expect(state[i].index).toBeGreaterThan(state[i - 1].index);
    }
  });

  it('fetch resolves first, then SSE pushes new events: no duplicates, no loss', () => {
    const seen = new Set<number>();
    let state: SessionEvent[] = [];

    // Initial fetch returns events 0..4.
    state = mergeSessionEvents(
      state,
      [ev(0), ev(1), ev(2), ev(3), ev(4)],
      seen,
    );
    expect(state.map((e) => e.index)).toEqual([0, 1, 2, 3, 4]);

    // SSE then pushes 5 and 6 — the live tail.
    state = mergeSessionEvents(state, [ev(5)], seen);
    state = mergeSessionEvents(state, [ev(6)], seen);

    expect(state).toHaveLength(7);
    expect(state.map((e) => e.index)).toEqual([0, 1, 2, 3, 4, 5, 6]);
  });

  it('drops duplicates by index across overlapping batches', () => {
    const seen = new Set<number>();
    let state: SessionEvent[] = [];
    // SSE pushes 3, then fetch returns 1..4 (overlap on 3, plus a stale 1/2).
    state = mergeSessionEvents(state, [ev(3)], seen);
    state = mergeSessionEvents(state, [ev(1), ev(2), ev(3), ev(4)], seen);
    expect(state.map((e) => e.index)).toEqual([1, 2, 3, 4]);
    // The shared `seen` set carries the dedupe state across calls.
    expect(seen.has(3)).toBe(true);
  });

  it('sorts out-of-order arrivals by index so display stays monotonic', () => {
    const seen = new Set<number>();
    // SSE delivers events out of order (e.g. retry replay) — the fix sorts
    // before returning so the timeline UI never shows them backwards.
    const state = mergeSessionEvents([], [ev(2), ev(0), ev(1)], seen);
    expect(state.map((e) => e.index)).toEqual([0, 1, 2]);
  });

  it('does not mutate the input array', () => {
    const seen = new Set<number>();
    const prev = [ev(0)];
    const result = mergeSessionEvents(prev, [ev(1)], seen);
    expect(prev).toHaveLength(1);
    expect(result).not.toBe(prev);
  });
});
