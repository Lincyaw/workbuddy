import type { JSX } from 'preact';
import type { SessionDetail, SessionListItem } from '../api/sessions';
import { statusBadgeClass } from '../lib/format';

// DegradedSessionCard renders a red warning banner for sessions that never
// produced any events. Issue #275: surfaces the difference between "session
// ran and emitted nothing" and "session was aborted before it could write a
// single event" so operators stop confusing the two.
//
// The banner appears when:
//   * the API explicitly tagged the response degraded=true (no DB row, or
//     no events file with a terminal status), OR
//   * we got zero events back from a non-running session — the same
//     "looks normal but actually empty" scenario the issue describes.
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

  const reasonLabel = describeDegradedReason(
    meta.degraded_reason,
    apiDegraded,
    inferredEmpty,
  );
  const stderrExcerpt = (meta.stderr_summary || '').trim();
  const summary = (meta.summary || '').trim();

  return (
    <div class="wb-degraded-card" role="alert">
      <h2>⚠️ This session never produced any events</h2>
      <p>
        Reason: <strong>{reasonLabel}</strong>
      </p>
      <p>
        Reported status:{' '}
        <span class={statusBadgeClass(meta.status || 'aborted_before_start')}>
          {meta.status || 'aborted_before_start'}
        </span>{' '}
        · exit code <code>{meta.exit_code}</code>
      </p>
      {summary && <pre>{summary}</pre>}
      {!summary && stderrExcerpt && (
        <>
          <p>Stderr (excerpt):</p>
          <pre>{stderrExcerpt}</pre>
        </>
      )}
      {!summary && !stderrExcerpt && (
        <p>
          No summary or stderr was captured for this session — the timeline below is
          empty by design, not a UI bug.
        </p>
      )}
    </div>
  );
}

function describeDegradedReason(
  reason: string | undefined,
  apiDegraded: boolean,
  inferredEmpty: boolean,
): string {
  if (reason === 'no_db_row') {
    return 'No agent_sessions row exists — response synthesized from disk metadata.json';
  }
  if (reason === 'no_events_file') {
    return 'No events-v1.jsonl file was ever written for this session';
  }
  if (apiDegraded) return reason || 'API flagged this session as degraded';
  if (inferredEmpty) return 'Zero events recorded for a session that already finished';
  return 'unknown';
}

// SessionStatusBadge renders the per-row status pill plus a ⚠️ marker for
// degraded sessions so operators can spot rows where the API explicitly
// flagged degraded=true. Issue #275.
export function SessionStatusBadge({
  row,
}: {
  row: SessionListItem;
}): JSX.Element {
  const statusText =
    row.status === 'aborted_before_start'
      ? 'aborted_before_start'
      : row.task_status || row.status || 'unknown';
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
