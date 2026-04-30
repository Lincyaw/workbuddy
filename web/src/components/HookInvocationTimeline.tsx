import { useState } from 'preact/hooks';
import type { HookInvocation } from '../api/hooks';
import { formatTimestamp } from '../lib/format';

interface HookInvocationTimelineProps {
  invocations: HookInvocation[];
}

// HookInvocationTimeline renders the per-hook call history. Rows are
// collapsible: clicking a row toggles the stdout/stderr preview block.
export function HookInvocationTimeline({ invocations }: HookInvocationTimelineProps) {
  const [openIdx, setOpenIdx] = useState<number | null>(null);

  if (invocations.length === 0) {
    return (
      <div class="empty" data-testid="hook-timeline-empty">
        No invocations recorded yet. Trigger an event and reload this page.
      </div>
    );
  }

  return (
    <ul class="wb-hook-timeline" aria-label="Hook invocation history">
      {invocations.map((inv, i) => {
        const isOpen = openIdx === i;
        const result = inv.result || 'unknown';
        const dur = formatDuration(inv.duration_ms);
        return (
          <li key={`${inv.started_at}-${i}`} class={`wb-hook-inv k-${result}`}>
            <button
              type="button"
              class="wb-hook-inv-row"
              aria-expanded={isOpen}
              aria-controls={`wb-hook-inv-body-${i}`}
              onClick={() => setOpenIdx(isOpen ? null : i)}
            >
              <span class={`wb-hook-result wb-hook-result-${result}`}>{result}</span>
              <span class="ts">{formatTimestamp(inv.started_at) || inv.started_at}</span>
              <span class="dur" title="Duration (ms)">{dur}</span>
              <span class="evt">
                {inv.event_type ? <span class="code-chip">{inv.event_type}</span> : null}
                {inv.repo ? <span class="muted"> {inv.repo}</span> : null}
                {inv.issue_num ? <span class="muted"> #{inv.issue_num}</span> : null}
              </span>
            </button>
            {isOpen && (
              <div class="wb-hook-inv-body" id={`wb-hook-inv-body-${i}`}>
                {inv.error && (
                  <div class="wb-hook-inv-error">
                    <strong>error:</strong> <code>{inv.error}</code>
                  </div>
                )}
                <OutputBlock label="stdout" body={inv.stdout} />
                <OutputBlock label="stderr" body={inv.stderr} />
                {!inv.stdout && !inv.stderr && !inv.error && (
                  <div class="muted">No captured output.</div>
                )}
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
    <div class="wb-hook-inv-output">
      <div class="muted" style={{ fontSize: 11 }}>{label}:</div>
      <pre>{body}</pre>
    </div>
  );
}

function formatDuration(ms: number): string {
  if (ms < 1) return '<1 ms';
  if (ms < 1000) return `${ms} ms`;
  return `${(ms / 1000).toFixed(2)} s`;
}

export default HookInvocationTimeline;
