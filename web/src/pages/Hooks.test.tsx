import { describe, expect, it } from 'vitest';
import { extractRolloutOutcomes } from './Hooks';

describe('extractRolloutOutcomes', () => {
  it('reads rollout outcomes from the legacy events envelope shape', () => {
    const outcomes = extractRolloutOutcomes({
      events: [
        { payload: { decision: 'join_satisfied' } },
        { payload: { decision: 'min_successes_unmet' } },
        { payload: { decision: 'join_satisfied' } },
      ],
    });

    expect(outcomes).toEqual([true, false, true]);
  });

  it('reads rollout outcomes from the current bare-array /api/v1/events shape', () => {
    const outcomes = extractRolloutOutcomes([
      { payload: { decision: 'join_satisfied' } },
      { payload: { decision: 'min_successes_unmet' } },
      { payload: { decision: 'join_satisfied' } },
    ]);

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
