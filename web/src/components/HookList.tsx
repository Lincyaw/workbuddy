import type { HookListEntry } from '../api/hooks';
import { errorRatePercent } from '../api/hooks';

interface HookListProps {
  hooks: HookListEntry[];
  onSelect: (name: string) => void;
}

// HookList renders the registered-hooks table used by /hooks. The rows are
// clickable and route to /hooks/:name; an explicit View link gives keyboard
// users a focusable target.
export function HookList({ hooks, onSelect }: HookListProps) {
  return (
    <table class="clickable wb-hooks-table">
      <thead>
        <tr>
          <th>Name</th>
          <th>Events</th>
          <th>Action</th>
          <th>Calls</th>
          <th>Errors</th>
          <th>Error rate</th>
          <th>State</th>
        </tr>
      </thead>
      <tbody>
        {hooks.map((h) => {
          const total = h.successes + h.failures;
          const rate = errorRatePercent(h.successes, h.failures);
          const stateLabel = h.auto_disabled
            ? 'auto-disabled'
            : h.enabled
            ? 'enabled'
            : 'disabled';
          const stateClass = h.auto_disabled
            ? 'badge failed'
            : h.enabled
            ? 'badge done'
            : 'badge queued';
          return (
            <tr
              key={h.name}
              onClick={(e) => {
                if ((e.target as HTMLElement).closest('a, button')) return;
                onSelect(h.name);
              }}
            >
              <td>
                <a
                  href={`/hooks/${encodeURIComponent(h.name)}`}
                  class="code-chip"
                  onClick={(e) => {
                    e.preventDefault();
                    onSelect(h.name);
                  }}
                >
                  {h.name}
                </a>
              </td>
              <td>
                {h.events.length === 0 ? (
                  <span class="muted">—</span>
                ) : (
                  h.events.map((ev) => (
                    <span class="wb-event-chip" key={ev}>
                      {ev}
                    </span>
                  ))
                )}
              </td>
              <td><span class="code-chip">{h.action_type}</span></td>
              <td title="Total invocations since dispatcher start (or last reload)">
                {total}
              </td>
              <td class={h.failures > 0 ? 'cell-stuck' : ''}>{h.failures}</td>
              <td class={rate > 50 ? 'cell-stuck' : ''}>{rate}%</td>
              <td>
                <span class={stateClass}>{stateLabel}</span>
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

export default HookList;
