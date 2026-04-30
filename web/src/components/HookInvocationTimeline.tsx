import { useState } from 'preact/hooks';
import type { HookInvocation } from '../api/hooks';
import { formatTimestamp } from '../lib/format';
import { EmptyState } from './EmptyState';

interface HookInvocationTimelineProps {
  invocations: HookInvocation[];
}

function resultBadge(result: string): string {
  switch (result) {
    case 'success':
      return 'wb-badge wb-badge-success';
    case 'failure':
      return 'wb-badge wb-badge-danger';
    case 'overflow':
      return 'wb-badge wb-badge-warning';
    default:
      return 'wb-badge wb-badge-neutral';
  }
}

export function HookInvocationTimeline({ invocations }: HookInvocationTimelineProps) {
  const [openIdx, setOpenIdx] = useState<number | null>(null);

  if (invocations.length === 0) {
    return (
      <div data-testid="hook-timeline-empty"><EmptyState
        icon="[]"
        title="No invocations recorded"
        copy="Trigger a hook event, then reload this page to inspect stdout, stderr, and error details."
      /></div>
    );
  }

  return (
    <ul class="wb-hook-timeline" aria-label="Hook invocation history">
      {invocations.map((invocation, index) => {
        const isOpen = openIdx === index;
        const result = invocation.result || 'unknown';
        const duration = formatDuration(invocation.duration_ms);
        return (
          <li key={`${invocation.started_at}-${index}`} class="wb-hook-inv">
            <button
              type="button"
              class="wb-hook-inv-row"
              aria-expanded={isOpen}
              aria-controls={`wb-hook-inv-body-${index}`}
              onClick={() => setOpenIdx(isOpen ? null : index)}
            >
              <span class={`wb-hook-result ${resultBadge(result)}`}>{result}</span>
              <span class="wb-time">{formatTimestamp(invocation.started_at) || invocation.started_at}</span>
              <span class="wb-num" title="Duration in milliseconds">{duration}</span>
              <span class="wb-hook-inv-row__summary">
                {invocation.event_type ? <span class="wb-code-pill">{invocation.event_type}</span> : null}
                {invocation.repo ? <span class="wb-muted">{invocation.repo}</span> : null}
                {invocation.issue_num ? <span class="wb-muted">#{invocation.issue_num}</span> : null}
              </span>
            </button>
            {isOpen && (
              <div class="wb-hook-inv-body" id={`wb-hook-inv-body-${index}`}>
                {invocation.error && (
                  <div class="wb-alert wb-alert--danger wb-hook-inline-alert">
                    <strong>Error</strong> <code class="wb-code-inline">{invocation.error}</code>
                  </div>
                )}
                <OutputBlock label="stdout" body={invocation.stdout} />
                <OutputBlock label="stderr" body={invocation.stderr} />
                {!invocation.stdout && !invocation.stderr && !invocation.error && <div class="wb-muted">No captured output.</div>}
              </div>
            )}
          </li>
        );
      })}
    </ul>
  );
}

function OutputBlock({ label, body }: { label: string; body?: string }) {
  if (!body) return null;
  return (
    <div class="wb-stack-xs">
      <div class="wb-code-label">{label}</div>
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
