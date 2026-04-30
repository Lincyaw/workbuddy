import type { SessionEvent } from '../api/sessions';

/**
 * Merge a batch of incoming session events into the existing list.
 *
 * The SessionDetail page subscribes to two asynchronous sources — the initial
 * REST fetch and the SSE tail — that can interleave in either order. Both
 * paths funnel through this helper so:
 *
 *   - duplicates (by `index`) are dropped using the shared `seen` set,
 *   - the result is sorted by `index` so out-of-order arrivals still render
 *     monotonically,
 *   - the existing array is preserved by value (no in-place mutation).
 *
 * The `seen` set is mutated in place so callers can keep using it as a fast
 * dedupe guard between calls.
 */
export function mergeSessionEvents(
  prev: SessionEvent[],
  incoming: SessionEvent[],
  seen: Set<number>,
): SessionEvent[] {
  const merged = prev.slice();
  for (const ev of incoming) {
    if (seen.has(ev.index)) continue;
    seen.add(ev.index);
    merged.push(ev);
  }
  merged.sort((a, b) => a.index - b.index);
  return merged;
}
