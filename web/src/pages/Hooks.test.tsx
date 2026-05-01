import { describe, expect, it } from 'vitest';
import { extractRolloutOutcomes } from './Hooks';

describe('extractRolloutOutcomes', () => {
  it('reads rollout outcomes from the events envelope returned by /api/v1/events', () => {
    const outcomes = extractRolloutOutcomes({
      events: [
        { payload: { decision: 'join_satisfied' } },
        { payload: { decision: 'min_successes_unmet' } },
        { payload: { decision: 'join_satisfied' } },
      ],
    });

    expect(outcomes).toEqual([true, false, true]);
  });

  it('returns only the latest 50 rollout-group results', () => {
    const outcomes = extractRolloutOutcomes({
      events: Array.from({ length: 55 }, (_, index) => ({
        payload: { decision: index % 2 === 0 ? 'join_satisfied' : 'min_successes_unmet' },
      })),
    });

    expect(outcomes).toHaveLength(50);
    expect(outcomes[0]).toBe(false);
    expect(outcomes[49]).toBe(true);
  });
});
