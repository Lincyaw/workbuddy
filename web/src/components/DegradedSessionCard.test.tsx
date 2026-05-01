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
    status: 'failed',
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

  it('describes no_events_file reason in the red warning', () => {
    const meta = makeMeta({
      degraded: true,
      degraded_reason: 'no_events_file',
      status: 'failed',
      exit_code: 1,
    });
    const { container, getByRole } = render(
      <DegradedSessionCard meta={meta} eventsTotal={0} />,
    );
    expect(getByRole('alert')).not.toBeNull();
    expect(container.textContent).toContain(
      'No events-v1.jsonl file was ever written',
    );
    expect(container.querySelector('.wb-degraded-card-info')).toBeNull();
  });

  it('renders a polite info panel for worker_offline (gray, role=status)', () => {
    const meta = makeMeta({
      degraded: true,
      degraded_reason: 'worker_offline',
      status: 'running',
      worker_id: 'worker-7',
      exit_code: 0,
    });
    const { container, getByRole, queryByRole } = render(
      <DegradedSessionCard meta={meta} eventsTotal={0} />,
    );
    // worker_offline must be a status (info) banner, never the alert
    // (red) one.
    expect(getByRole('status')).not.toBeNull();
    expect(queryByRole('alert')).toBeNull();
    expect(container.querySelector('.wb-degraded-card-info')).not.toBeNull();
    expect(container.textContent).toContain('worker is currently unreachable');
    expect(container.textContent).toContain('worker-7');
    // Must not paint the "never produced any events" message — that's
    // the wrong story for an offline worker.
    expect(container.textContent).not.toContain('never produced any events');
  });

  it('falls back gracefully on the legacy no_db_row reason', () => {
    const meta = makeMeta({
      degraded: true,
      degraded_reason: 'no_db_row',
      status: 'aborted_before_start',
      summary: 'legacy synthesized summary',
    });
    const { container } = render(
      <DegradedSessionCard meta={meta} eventsTotal={0} />,
    );
    expect(container.querySelector('.wb-degraded-card')).not.toBeNull();
    expect(container.textContent).toContain('legacy disk-only synthesis');
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

  it('shows the warning marker when degraded=no_events_file', () => {
    const row = makeRow({
      degraded: true,
      degraded_reason: 'no_events_file',
      status: 'failed',
    });
    const { container } = render(<SessionStatusBadge row={row} />);
    const marker = container.querySelector('.wb-degraded-marker');
    expect(marker).not.toBeNull();
    expect(marker?.textContent).toContain('⚠️');
    const badge = container.querySelector('.wb-badge');
    expect(badge?.classList.contains('wb-badge-degraded')).toBe(true);
  });

  it('renders a gray "offline" pill (not the red ⚠️) for worker_offline', () => {
    const row = makeRow({
      degraded: true,
      degraded_reason: 'worker_offline',
      worker_id: 'worker-9',
    });
    const { container } = render(<SessionStatusBadge row={row} />);
    expect(container.querySelector('.wb-degraded-marker')).toBeNull();
    const badge = container.querySelector('.wb-badge');
    expect(badge?.classList.contains('wb-badge-offline')).toBe(true);
    expect(badge?.textContent).toBe('offline');
  });

  it('renders a normal badge with no marker when not degraded', () => {
    const row = makeRow({ status: 'completed', task_status: 'completed' });
    const { container } = render(<SessionStatusBadge row={row} />);
    expect(container.querySelector('.wb-degraded-marker')).toBeNull();
    expect(container.querySelector('.wb-badge')?.textContent).toBe('completed');
  });
});
