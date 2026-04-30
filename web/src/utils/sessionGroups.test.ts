import { describe, it, expect } from 'vitest';
import type { SessionListItem } from '../api/sessions';
import {
  distinctValues,
  groupSessionsByIssue,
  inferRole,
} from './sessionGroups';

function mk(overrides: Partial<SessionListItem>): SessionListItem {
  return {
    session_id: 's',
    repo: 'owner/repo',
    issue_num: 1,
    agent_name: 'dev-agent',
    attempt: 1,
    status: 'completed',
    exit_code: 0,
    duration: 0,
    created_at: '2026-04-29T10:00:00Z',
    ...overrides,
  };
}

describe('inferRole', () => {
  it('classifies review agents (priority over dev substring)', () => {
    expect(inferRole('review-agent')).toBe('review');
    expect(inferRole('codex-reviewer')).toBe('review');
    // 'dev-review-bot' includes 'review' so it must land on review.
    expect(inferRole('dev-review-bot')).toBe('review');
  });
  it('classifies dev agents', () => {
    expect(inferRole('dev-agent')).toBe('dev');
    expect(inferRole('claude-dev')).toBe('dev');
  });
  it('falls back to other', () => {
    expect(inferRole('docs-bot')).toBe('other');
    expect(inferRole('')).toBe('other');
  });
});

describe('groupSessionsByIssue', () => {
  it('groups by issue and assigns dev/review cycles in chronological order', () => {
    const sessions: SessionListItem[] = [
      mk({ session_id: 'd1', issue_num: 10, agent_name: 'dev-agent', created_at: '2026-04-29T10:00:00Z' }),
      mk({ session_id: 'r1', issue_num: 10, agent_name: 'review-agent', created_at: '2026-04-29T11:00:00Z' }),
      mk({ session_id: 'd2', issue_num: 10, agent_name: 'dev-agent', created_at: '2026-04-29T12:00:00Z' }),
      mk({ session_id: 'r2', issue_num: 10, agent_name: 'review-agent', created_at: '2026-04-29T13:00:00Z' }),
      mk({ session_id: 'd-other', issue_num: 11, agent_name: 'dev-agent', created_at: '2026-04-29T09:00:00Z' }),
    ];

    const groups = groupSessionsByIssue(sessions);

    // Issue 10 was active most recently (latest created_at), so first.
    expect(groups.map((g) => g.issueNum)).toEqual([10, 11]);

    const g10 = groups[0];
    expect(g10.sessions.map((s) => `${s.role}-c${s.cycle}-${s.session_id}`)).toEqual([
      'dev-c1-d1',
      'review-c1-r1',
      'dev-c2-d2',
      'review-c2-r2',
    ]);

    expect(groups[1].sessions).toHaveLength(1);
    expect(groups[1].sessions[0].role).toBe('dev');
    expect(groups[1].sessions[0].cycle).toBe(1);
  });

  it('handles unsorted input and out-of-order cycles', () => {
    const sessions: SessionListItem[] = [
      mk({ session_id: 'r2', issue_num: 5, agent_name: 'review-agent', created_at: '2026-04-29T13:00:00Z' }),
      mk({ session_id: 'd1', issue_num: 5, agent_name: 'dev-agent', created_at: '2026-04-29T10:00:00Z' }),
      mk({ session_id: 'd2', issue_num: 5, agent_name: 'dev-agent', created_at: '2026-04-29T12:00:00Z' }),
      mk({ session_id: 'r1', issue_num: 5, agent_name: 'review-agent', created_at: '2026-04-29T11:00:00Z' }),
    ];
    const [g] = groupSessionsByIssue(sessions);
    expect(g.sessions.map((s) => s.session_id)).toEqual(['d1', 'r1', 'd2', 'r2']);
    expect(g.sessions.map((s) => s.cycle)).toEqual([1, 1, 2, 2]);
  });

  it('classifies non-dev/review agents as "other" and sorts them last', () => {
    const sessions: SessionListItem[] = [
      mk({ session_id: 'x', issue_num: 7, agent_name: 'docs-bot', created_at: '2026-04-29T09:00:00Z' }),
      mk({ session_id: 'd', issue_num: 7, agent_name: 'dev-agent', created_at: '2026-04-29T10:00:00Z' }),
    ];
    const [g] = groupSessionsByIssue(sessions);
    expect(g.sessions[0].role).toBe('dev');
    expect(g.sessions[1].role).toBe('other');
  });

  it('keeps same issue_num across different repos in separate buckets', () => {
    const sessions: SessionListItem[] = [
      mk({
        session_id: 'a-d',
        repo: 'owner/a',
        issue_num: 42,
        agent_name: 'dev-agent',
        created_at: '2026-04-29T10:00:00Z',
      }),
      mk({
        session_id: 'a-r',
        repo: 'owner/a',
        issue_num: 42,
        agent_name: 'review-agent',
        created_at: '2026-04-29T11:00:00Z',
      }),
      mk({
        session_id: 'b-d',
        repo: 'owner/b',
        issue_num: 42,
        agent_name: 'dev-agent',
        created_at: '2026-04-29T12:00:00Z',
      }),
    ];

    const groups = groupSessionsByIssue(sessions);
    // Two distinct buckets even though both have issue_num=42.
    expect(groups).toHaveLength(2);
    const byRepo = new Map(groups.map((g) => [g.repo, g]));
    const a = byRepo.get('owner/a');
    const b = byRepo.get('owner/b');
    expect(a).toBeDefined();
    expect(b).toBeDefined();
    expect(a!.sessions.map((s) => s.session_id)).toEqual(['a-d', 'a-r']);
    expect(b!.sessions.map((s) => s.session_id)).toEqual(['b-d']);
    // Both buckets must report the shared issue number unchanged.
    expect(a!.issueNum).toBe(42);
    expect(b!.issueNum).toBe(42);
  });

  it('uses rollout numbering when rollout metadata is present', () => {
    const sessions: SessionListItem[] = [
      mk({
        session_id: 'r2',
        issue_num: 12,
        agent_name: 'dev-agent',
        rollout_index: 2,
        rollouts_total: 3,
        created_at: '2026-04-29T10:00:00Z',
      }),
      mk({
        session_id: 'r1',
        issue_num: 12,
        agent_name: 'dev-agent',
        rollout_index: 1,
        rollouts_total: 3,
        created_at: '2026-04-29T09:00:00Z',
      }),
    ];

    const [group] = groupSessionsByIssue(sessions);
    expect(group.sessions.map((s) => s.cycle)).toEqual([1, 2]);
    expect(group.sessions.map((s) => s.rolloutLabel)).toEqual([
      'rollout 1/3',
      'rollout 2/3',
    ]);
  });
});

describe('distinctValues', () => {
  it('returns unique sorted non-empty values', () => {
    const items = [
      mk({ repo: 'a/b' }),
      mk({ repo: 'c/d' }),
      mk({ repo: 'a/b' }),
      mk({ repo: '' }),
    ];
    expect(distinctValues(items, (s) => s.repo)).toEqual(['a/b', 'c/d']);
  });
});
