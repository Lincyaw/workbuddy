import type { HookListEntry } from '../api/hooks';
import { errorRatePercent } from '../api/hooks';

interface HookListProps {
  hooks: HookListEntry[];
  latencySeries?: Record<string, number[]>;
  onSelect: (name: string) => void;
}

function hookState(entry: HookListEntry): { label: string; tone: string } {
  if (entry.auto_disabled) return { label: 'auto-disabled', tone: 'danger' };
  if (entry.enabled) return { label: 'armed', tone: 'success' };
  return { label: 'disabled', tone: 'neutral' };
}

function Sparkline({ values }: { values: number[] }) {
  if (values.length === 0) {
    return <div class="text-[12px] text-text-tertiary">no samples</div>;
  }
  const max = Math.max(...values, 1);
  const step = values.length === 1 ? 0 : 116 / (values.length - 1);
  const points = values
    .map((value, index) => `${2 + index * step},${30 - Math.round((value / max) * 24)}`)
    .join(' ');
  return (
    <svg viewBox="0 0 120 32" class="wb-sparkline" aria-hidden="true">
      <polyline fill="none" stroke="var(--border-strong)" stroke-width="1" points="0,30 120,30" />
      <polyline fill="none" stroke="var(--accent)" stroke-width="2" points={points} />
    </svg>
  );
}

export function HookList({ hooks, latencySeries = {}, onSelect }: HookListProps) {
  return (
    <>
      <div class="wb-table-wrap">
        <table class="wb-table">
          <thead>
            <tr>
              <th>name</th>
              <th>event glob</th>
              <th>last result</th>
              <th>latency</th>
              <th>calls</th>
              <th>error rate</th>
            </tr>
          </thead>
          <tbody>
            {hooks.map((hook) => {
              const total = hook.successes + hook.failures;
              const rate = errorRatePercent(hook.successes, hook.failures);
              const state = hookState(hook);
              const lastResult = hook.failures > 0 && hook.last_failure_at ? 'failure' : hook.successes > 0 ? 'success' : state.label;
              return (
                <tr key={hook.name} onClick={() => onSelect(hook.name)}>
                  <td>
                    <div class="grid gap-2">
                      <strong class="text-[15px]">{hook.name}</strong>
                      <span class="wb-mono text-[12px] text-text-secondary">{hook.action_type}</span>
                    </div>
                  </td>
                  <td><span class="wb-id-pill">{hook.events.join(', ') || '*'}</span></td>
                  <td><span class={`wb-badge wb-badge--${state.tone === 'success' && lastResult === 'success' ? 'success' : lastResult === 'failure' ? 'danger' : state.tone}`}>{lastResult}</span></td>
                  <td><Sparkline values={latencySeries[hook.name] || []} /></td>
                  <td class="wb-num">{total}</td>
                  <td class="wb-num">{rate}%</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      <div class="wb-mobile-card-list">
        {hooks.map((hook) => {
          const total = hook.successes + hook.failures;
          const rate = errorRatePercent(hook.successes, hook.failures);
          const state = hookState(hook);
          return (
            <button type="button" class="wb-mobile-card text-left" key={hook.name} onClick={() => onSelect(hook.name)}>
              <div class="flex items-center justify-between gap-3">
                <strong>{hook.name}</strong>
                <span class={`wb-badge wb-badge--${state.tone}`}>{state.label}</span>
              </div>
              <div class="mt-2 text-[12px] text-text-secondary">{hook.events.join(', ') || '*'}</div>
              <div class="mt-3 flex items-center justify-between gap-3 text-[12px]">
                <span>{total} calls</span>
                <span>{rate}% error</span>
              </div>
              <div class="mt-3"><Sparkline values={latencySeries[hook.name] || []} /></div>
            </button>
          );
        })}
      </div>
    </>
  );
}

export default HookList;
