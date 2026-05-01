import { render, screen } from '@testing-library/preact';
import { describe, expect, it } from 'vitest';
import type { RolloutGroup } from '../api/types';
import { RolloutGroupPanel } from './RolloutGroupPanel';

function makeGroup(overrides: Partial<RolloutGroup> = {}): RolloutGroup {
  return {
    group_id: 'group-294',
    rollouts_total: 3,
    members: [
      {
        rollout_index: 1,
        pr_number: 401,
        status: 'completed',
        session_id: 'session-1',
        worker_id: 'worker-a',
        started_at: '2026-05-01T10:00:00Z',
        ended_at: '2026-05-01T10:05:00Z',
        duration_seconds: 300,
      },
      {
        rollout_index: 2,
        pr_number: 402,
        status: 'completed',
        session_id: 'session-2',
        worker_id: 'worker-b',
        started_at: '2026-05-01T10:01:00Z',
        ended_at: '2026-05-01T10:06:00Z',
        duration_seconds: 300,
      },
      {
        rollout_index: 3,
        pr_number: 403,
        status: 'completed',
        session_id: 'session-3',
        worker_id: 'worker-c',
        started_at: '2026-05-01T10:02:00Z',
        ended_at: '2026-05-01T10:07:00Z',
        duration_seconds: 300,
      },
    ],
    synth_outcome: {
      decision: 'pick',
      chosen_pr: 402,
      chosen_rollout_index: 2,
      reason: 'highest signal',
      ts: '2026-05-01T10:08:00Z',
    },
    ...overrides,
  };
}

describe('RolloutGroupPanel', () => {
  it('renders all-success rollouts with a synth pick row', () => {
    render(
      <RolloutGroupPanel
        repo="owner/repo"
        issueNum={294}
        group={makeGroup()}
        compareHref="/issues/owner/repo/294/rollouts/compare?a=1&b=2"
      />,
    );

    expect(screen.getByText(/3\/3 succeeded/i)).toBeTruthy();
    expect(screen.getByText('synth')).toBeTruthy();
    expect(screen.getByText('pick')).toBeTruthy();
    expect(screen.getByText('chose #402')).toBeTruthy();
  });

  it('renders mixed rollout status when min successes were met', () => {
    const group = makeGroup({
      members: [
        makeGroup().members[0],
        { ...makeGroup().members[1], status: 'failed' },
        makeGroup().members[2],
      ],
    });
    render(<RolloutGroupPanel repo="owner/repo" issueNum={294} group={group} />);

    expect(screen.getByText(/2\/3 succeeded/i)).toBeTruthy();
    expect(screen.getByText('failed')).toBeTruthy();
  });

  it('renders escalated synth outcomes', () => {
    const group = makeGroup({
      synth_outcome: {
        decision: 'escalate',
        reason: 'manual review requested',
        ts: '2026-05-01T10:08:00Z',
      },
    });
    render(<RolloutGroupPanel repo="owner/repo" issueNum={294} group={group} />);

    expect(screen.getByText('escalate')).toBeTruthy();
    expect(screen.getByText('manual review requested')).toBeTruthy();
  });

  it('hides itself for non-rollout issues', () => {
    const { container } = render(
      <RolloutGroupPanel
        repo="owner/repo"
        issueNum={294}
        group={{ group_id: '', rollouts_total: 1, members: [], synth_outcome: null }}
      />,
    );

    expect(container.innerHTML).toBe('');
  });
});
