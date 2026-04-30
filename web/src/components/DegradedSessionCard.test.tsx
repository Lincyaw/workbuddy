import { render } from '@testing-library/preact';
import { describe, expect, it } from 'vitest';
import {
  DegradedSessionCard,
  SessionStatusBadge,
} from './DegradedSessionCard';
import type { SessionDetail, SessionListItem } from '../api/sessions';

function makeMeta(overrides: Partial<SessionDetail> = {}): SessionDetail {
  return {
    session_id: 'session-test',
    repo: 'owner/repo',
    issue_num: 1,
    agent_name: 'dev-agent',
    attempt: 1,
    status: 'aborted_before_start',
    exit_code: -1,
    duration: 0,
    created_at: '2026-04-30T08:00:00Z',
    artifact_paths: {},
    ...overrides,
  };
}

describe('DegradedSessionCard', () => {
  it('renders nothing when meta is null', () => {
    const { container } = render(
      <DegradedSessionCard meta={null} eventsTotal={null} />,
    );
    expect(container.querySelector('.wb-degraded-card')).toBeNull();
  });

  it('renders nothing for a normal completed session with events', () => {
    const meta = makeMeta({ status: 'completed', exit_code: 0 });
    const { container } = render(
      <DegradedSessionCard meta={meta} eventsTotal={42} />,
    );
    expect(container.querySelector('.wb-degraded-card')).toBeNull();
  });

  it('renders the warning when API flagged degraded=true with no_db_row', () => {
    const meta = makeMeta({
      degraded: true,
      degraded_reason: 'no_db_row',
      summary: '## Session Summary (no session file)\n\n- Exit code: -1\n',
      stderr_summary: 'runtime: claude-oneshot: context canceled',
    });
    const { container, getByRole } = render(
      <DegradedSessionCard meta={meta} eventsTotal={0} />,
    );
    expect(getByRole('alert')).not.toBeNull();
    expect(container.textContent).toContain('never produced any events');
    expect(container.textContent).toContain(
      'No agent_sessions row exists',
    );
    expect(container.textContent).toContain('aborted_before_start');
    expect(container.textContent).toContain('-1');
    expect(container.textContent).toContain('Session Summary (no session file)');
  });

  it('renders the warning for inferred-empty (zero events on a finished session)', () => {
    const meta = makeMeta({ status: 'completed', exit_code: 0, degraded: false });
    const { container } = render(
      <DegradedSessionCard meta={meta} eventsTotal={0} />,
    );
    expect(container.querySelector('.wb-degraded-card')).not.toBeNull();
    expect(container.textContent).toContain(
      'Zero events recorded for a session that already finished',
    );
  });

  it('does not warn for running sessions with no events yet', () => {
    const meta = makeMeta({ status: 'running', exit_code: 0, degraded: false });
    const { container } = render(
      <DegradedSessionCard meta={meta} eventsTotal={0} />,
    );
    expect(container.querySelector('.wb-degraded-card')).toBeNull();
  });

  it('describes no_events_file reason', () => {
    const meta = makeMeta({
      degraded: true,
      degraded_reason: 'no_events_file',
      status: 'failed',
      exit_code: 1,
    });
    const { container } = render(
      <DegradedSessionCard meta={meta} eventsTotal={0} />,
    );
    expect(container.textContent).toContain(
      'No events-v1.jsonl file was ever written',
    );
  });
});

describe('SessionStatusBadge', () => {
  function makeRow(
    overrides: Partial<SessionListItem> = {},
  ): SessionListItem {
    return {
      session_id: 'session-x',
      repo: 'owner/repo',
      issue_num: 1,
      agent_name: 'dev-agent',
      attempt: 1,
      status: 'completed',
      exit_code: 0,
      duration: 0,
      created_at: '2026-04-30T08:00:00Z',
      ...overrides,
    };
  }

  it('shows the warning marker when degraded=true', () => {
    const row = makeRow({
      degraded: true,
      degraded_reason: 'no_db_row',
      status: 'aborted_before_start',
    });
    const { container } = render(<SessionStatusBadge row={row} />);
    const marker = container.querySelector('.wb-degraded-marker');
    expect(marker).not.toBeNull();
    expect(marker?.textContent).toContain('⚠️');
    const badge = container.querySelector('.wb-badge');
    expect(badge?.classList.contains('wb-badge-degraded')).toBe(true);
    expect(badge?.textContent).toBe('aborted_before_start');
  });

  it('renders a normal badge with no marker when not degraded', () => {
    const row = makeRow({ status: 'completed', task_status: 'completed' });
    const { container } = render(<SessionStatusBadge row={row} />);
    expect(container.querySelector('.wb-degraded-marker')).toBeNull();
    expect(container.querySelector('.wb-badge')?.textContent).toBe('completed');
  });
});
