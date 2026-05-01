import type { JSX } from 'preact';
import type { SessionDetail, SessionListItem } from '../api/sessions';
import { statusBadgeClass } from '../lib/format';

// DegradedSessionCard renders a banner explaining why a session is
// degraded. Phase 3 of the session-data ownership refactor (REQ-122)
// distinguishes three cases:
//
//   * 'worker_offline' — gray, polite. The data lives on a worker that
//                         the coordinator could not dial (HTTP 503 from
//                         the proxy). Common during a worker restart;
//                         not a failure of the session itself.
//   * 'no_events_file' — red, the existing warning. The DB row exists
//                         and the session reached a terminal status,
//                         but events-v1.jsonl was never written. Real
//                         signal: the agent crashed before producing
//                         any tool calls.
//   * 'no_db_row'      — legacy. Should not appear post-Phase-3 (the
//                         disk-only synthesis path has been deleted).
//                         Kept here as a defensive fallback so old
//                         payloads still render reasonably.
//
// The banner also covers the inferred-empty case: a non-running session
// with zero events even though the API didn't tag it degraded.
export function DegradedSessionCard({
  meta,
  eventsTotal,
}: {
  meta: SessionDetail | null;
  eventsTotal: number | null;
}): JSX.Element | null {
  if (!meta) return null;
  const apiDegraded = meta.degraded === true;
  const status = (meta.status || '').toLowerCase();
  const inferredEmpty =
    eventsTotal !== null &&
    eventsTotal === 0 &&
    status !== '' &&
    status !== 'running' &&
    status !== 'pending';
  if (!apiDegraded && !inferredEmpty) return null;

  const reason = (meta.degraded_reason || '').toLowerCase();

  if (reason === 'worker_offline') {
    return (
      <div class="wb-degraded-card wb-degraded-card-info" role="status">
        <h2>This worker is currently unreachable</h2>
        <p>
          The session data lives on{' '}
          <code>{meta.worker_id || 'an unknown worker'}</code>, which the
          coordinator could not contact. Sessions return when the worker
          reconnects.
        </p>
        <p>
          Reported status:{' '}
          <span class={statusBadgeClass(meta.status || 'unknown')}>
            {meta.status || 'unknown'}
          </span>
        </p>
      </div>
    );
  }

  return (
    <div class="wb-degraded-card" role="alert">
      <h2>⚠️ This session never produced any events</h2>
      <p>
        Reason:{' '}
        <strong>
          {describeDegradedReason(reason, apiDegraded, inferredEmpty)}
        </strong>
      </p>
      <p>
        Reported status:{' '}
        <span class={statusBadgeClass(meta.status || 'failed')}>
          {meta.status || 'unknown'}
        </span>{' '}
        · exit code <code>{meta.exit_code}</code>
      </p>
      {meta.summary && <pre>{meta.summary.trim()}</pre>}
      {!meta.summary && meta.stderr_summary && (
        <>
          <p>Stderr (excerpt):</p>
          <pre>{meta.stderr_summary.trim()}</pre>
        </>
      )}
      {!meta.summary && !meta.stderr_summary && (
        <p>
          No summary or stderr was captured for this session — the timeline below is
          empty by design, not a UI bug.
        </p>
      )}
    </div>
  );
}

function describeDegradedReason(
  reason: string,
  apiDegraded: boolean,
  inferredEmpty: boolean,
): string {
  if (reason === 'no_events_file') {
    return 'No events-v1.jsonl file was ever written for this session';
  }
  if (reason === 'no_db_row') {
    // Legacy reason — Phase 3 deleted the disk-only synthesis path that
    // produced this. Shown verbatim only if a stale coordinator still
    // emits it.
    return 'No DB row exists for this session (legacy disk-only synthesis)';
  }
  if (apiDegraded) return reason || 'API flagged this session as degraded';
  if (inferredEmpty) return 'Zero events recorded for a session that already finished';
  return 'unknown';
}

// SessionStatusBadge renders the per-row status pill plus a marker for
// degraded sessions. Phase 3 (REQ-122): worker_offline rows render as a
// gray pill instead of the red ⚠️ marker — the worker being briefly
// unreachable isn't the session's fault.
export function SessionStatusBadge({
  row,
}: {
  row: SessionListItem;
}): JSX.Element {
  const reason = (row.degraded_reason || '').toLowerCase();
  const offline = reason === 'worker_offline';
  const statusText =
    row.status === 'aborted_before_start'
      ? 'aborted_before_start'
      : row.task_status || row.status || 'unknown';

  if (offline) {
    return (
      <span title={`worker offline: ${row.worker_id || 'unknown'}`}>
        <span class="wb-badge wb-badge-offline">offline</span>
      </span>
    );
  }

  const cls = row.degraded
    ? 'wb-badge wb-badge-degraded'
    : statusBadgeClass(row.task_status || row.status);
  const title = row.degraded
    ? `degraded: ${row.degraded_reason || 'no events captured'}`
    : undefined;
  return (
    <span title={title}>
      {row.degraded && (
        <span
          class="wb-degraded-marker"
          aria-label="degraded session"
          title={title}
        >
          ⚠️
        </span>
      )}
      <span class={cls}>{statusText}</span>
    </span>
  );
}
