import { useState } from 'preact/hooks';
import type { HookInvocation } from '../api/hooks';
import { formatTimestamp } from '../lib/format';
import { EmptyState } from './EmptyState';

interface HookInvocationTimelineProps {
  invocations: HookInvocation[];
}

function resultTone(result: string): string {
  switch (result) {
    case 'success':
      return 'success';
    case 'failure':
      return 'danger';
    case 'overflow':
      return 'warning';
    default:
      return 'neutral';
  }
}

export function HookInvocationTimeline({ invocations }: HookInvocationTimelineProps) {
  const [openIdx, setOpenIdx] = useState<number | null>(null);

  if (invocations.length === 0) {
    return <div data-testid="hook-timeline-empty"><EmptyState glyph="timeline" title="no invocations recorded" copy="Trigger a hook event, then reload this page to inspect stdout, stderr, and error details." /></div>;
  }

  return (
    <div class="wb-timeline-rail">
      {invocations.map((invocation, index) => {
        const isOpen = openIdx === index;
        return (
          <div key={`${invocation.started_at}-${index}`} class="wb-event-row">
            <button
              type="button"
              class="wb-event-toggle"
              aria-expanded={isOpen}
              aria-controls={`hook-invocation-${index}`}
              onClick={() => setOpenIdx(isOpen ? null : index)}
            >
              <span class={`wb-badge wb-badge--${resultTone(invocation.result || 'neutral')}`}>{invocation.result || 'unknown'}</span>
              <span class="truncate">{invocation.event_type || 'hook invocation'} · {invocation.repo || 'repo unknown'}{invocation.issue_num ? ` #${invocation.issue_num}` : ''}</span>
              <span class="wb-time">{formatTimestamp(invocation.started_at)} · {formatDuration(invocation.duration_ms)}</span>
            </button>
            {isOpen ? (
              <div class="wb-event-body" id={`hook-invocation-${index}`}>
                {invocation.error ? <div class="mb-3 text-state-danger">{invocation.error}</div> : null}
                {invocation.stdout ? <OutputBlock label="stdout" body={invocation.stdout} /> : null}
                {invocation.stderr ? <OutputBlock label="stderr" body={invocation.stderr} /> : null}
                {!invocation.stdout && !invocation.stderr && !invocation.error ? <div class="text-text-secondary">No captured output.</div> : null}
              </div>
            ) : null}
          </div>
        );
      })}
    </div>
  );
}

function OutputBlock({ label, body }: { label: string; body?: string }) {
  if (!body) return null;
  return (
    <div class="grid gap-2">
      <div class="wb-faint">{label}</div>
      <pre class="wb-codeblock">{body}</pre>
    </div>
  );
}

function formatDuration(ms: number): string {
  if (ms < 1) return '<1 ms';
  if (ms < 1000) return `${ms} ms`;
  return `${(ms / 1000).toFixed(2)} s`;
}

export default HookInvocationTimeline;
