import type { JSX } from 'preact';
import type { SessionDetail, SessionListItem } from '../api/sessions';
import { statusBadgeClass } from '../lib/format';

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
    eventsTotal !== null && eventsTotal === 0 && status !== '' && status !== 'running' && status !== 'pending';
  if (!apiDegraded && !inferredEmpty) return null;

  const reasonLabel = describeDegradedReason(meta.degraded_reason, apiDegraded, inferredEmpty);
  const stderrExcerpt = (meta.stderr_summary || '').trim();
  const summary = (meta.summary || '').trim();

  return (
    <div class="wb-panel wb-degraded-card grid gap-3 border-state-warning/40 bg-[color-mix(in_srgb,var(--state-warning)_10%,var(--bg-panel))]" role="alert">
      <div class="flex items-center justify-between gap-3">
        <h2 class="m-0 text-[20px]">this session never produced any events</h2>
        <span class="wb-badge wb-badge--warning">degraded session</span>
      </div>
      <p class="m-0"><strong>reason</strong> {reasonLabel}</p>
      <p class="m-0">
        <strong>reported status</strong> <span class={statusBadgeClass(meta.status || 'aborted_before_start')}>{meta.status || 'aborted_before_start'}</span> · exit code <code class="wb-code-inline">{meta.exit_code}</code>
      </p>
      {summary ? <pre class="wb-codeblock">{summary}</pre> : null}
      {!summary && stderrExcerpt ? (
        <>
          <div class="wb-faint">stderr excerpt</div>
          <pre class="wb-codeblock">{stderrExcerpt}</pre>
        </>
      ) : null}
      {!summary && !stderrExcerpt ? <p class="m-0 text-text-secondary">No summary or stderr was captured for this session.</p> : null}
    </div>
  );
}

function describeDegradedReason(reason: string | undefined, apiDegraded: boolean, inferredEmpty: boolean): string {
  if (reason === 'no_db_row') {
    return 'No agent_sessions row exists - response synthesized from disk metadata.';
  }
  if (reason === 'no_events_file') {
    return 'No events-v1.jsonl file was ever written for this session.';
  }
  if (apiDegraded) return reason || 'API flagged this session as degraded';
  if (inferredEmpty) return 'Zero events recorded for a session that already finished';
  return 'unknown';
}

export function SessionStatusBadge({ row }: { row: SessionListItem }): JSX.Element {
  const statusText = row.status === 'aborted_before_start' ? 'aborted_before_start' : row.task_status || row.status || 'unknown';
  const cls = row.degraded ? 'wb-badge wb-badge--warning wb-badge-degraded' : statusBadgeClass(row.task_status || row.status);
  const title = row.degraded ? `degraded: ${row.degraded_reason || 'no events captured'}` : undefined;
  return (
    <span title={title} class="inline-flex items-center gap-1">
      {row.degraded ? <span class="wb-degraded-marker" aria-label="degraded session">⚠️</span> : null}
      <span class={cls}>{statusText}</span>
    </span>
  );
}
