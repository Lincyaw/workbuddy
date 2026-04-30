import { useState } from 'preact/hooks';
import type { HookInvocation } from '../api/hooks';
import { formatClock, formatTimestamp } from '../lib/format';

interface HookInvocationTimelineProps {
  invocations: HookInvocation[];
}

export function HookInvocationTimeline({ invocations }: HookInvocationTimelineProps) {
  const [openIndex, setOpenIndex] = useState<number | null>(null);

  if (invocations.length === 0) {
    return <div class="loading-copy">No invocations recorded yet.</div>;
  }

  return (
    <div class="timeline-list hook-timeline-list">
      {invocations.map((invocation, index) => {
        const isOpen = openIndex === index;
        return (
          <article key={`${invocation.started_at}:${index}`} class={`timeline-event kind-system${isOpen ? ' is-open' : ''}`}>
            <button
              type="button"
              class="timeline-row"
              aria-expanded={isOpen}
              onClick={() => setOpenIndex(isOpen ? null : index)}
            >
              <span class="timeline-kind">◦</span>
              <span class="timeline-title">
                {invocation.result} · {invocation.event_type || 'hook invocation'}
              </span>
              <span class="timeline-time">{formatClock(invocation.started_at)}</span>
            </button>
            {isOpen ? (
              <div class="timeline-body">
                <div class="timeline-body-meta">
                  <span class="mono-chip">{formatTimestamp(invocation.started_at, true)}</span>
                  <span class="mono-chip">{invocation.duration_ms}ms</span>
                  {invocation.repo ? <span class="mono-chip">{invocation.repo}</span> : null}
                  {invocation.issue_num ? <span class="mono-chip">#{invocation.issue_num}</span> : null}
                </div>
                {invocation.error ? <pre>{invocation.error}</pre> : null}
                {invocation.stdout ? <pre>{invocation.stdout}</pre> : null}
                {invocation.stderr ? <pre>{invocation.stderr}</pre> : null}
                {!invocation.error && !invocation.stdout && !invocation.stderr ? <pre>(no captured output)</pre> : null}
              </div>
            ) : null}
          </article>
        );
      })}
    </div>
  );
}

export default HookInvocationTimeline;
