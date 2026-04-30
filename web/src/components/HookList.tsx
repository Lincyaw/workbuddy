import type { HookListEntry } from '../api/hooks';
import { errorRatePercent } from '../api/hooks';

interface HookListProps {
  hooks: HookListEntry[];
  onSelect: (name: string) => void;
}

function hookState(entry: HookListEntry): { label: string; badgeClass: string } {
  if (entry.auto_disabled) return { label: 'auto-disabled', badgeClass: 'wb-badge wb-badge-warning' };
  if (entry.enabled) return { label: 'enabled', badgeClass: 'wb-badge wb-badge-success' };
  return { label: 'disabled', badgeClass: 'wb-badge wb-badge-neutral' };
}

export function HookList({ hooks, onSelect }: HookListProps) {
  return (
    <>
      <div class="wb-table-scroll wb-desktop-only">
        <table class="wb-table wb-hooks-table">
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
            {hooks.map((hook) => {
              const total = hook.successes + hook.failures;
              const rate = errorRatePercent(hook.successes, hook.failures);
              const state = hookState(hook);
              return (
                <tr
                  key={hook.name}
                  class="wb-row-link"
                  onClick={(e) => {
                    if ((e.target as HTMLElement).closest('a, button')) return;
                    onSelect(hook.name);
                  }}
                >
                  <td>
                    <a
                      href={`/hooks/${encodeURIComponent(hook.name)}`}
                      class="wb-code-pill"
                      onClick={(e) => {
                        e.preventDefault();
                        onSelect(hook.name);
                      }}
                    >
                      {hook.name}
                    </a>
                  </td>
                  <td>
                    {hook.events.length === 0 ? (
                      <span class="wb-muted">--</span>
                    ) : (
                      hook.events.map((ev) => (
                        <span class="wb-event-pill" key={ev}>{ev}</span>
                      ))
                    )}
                  </td>
                  <td><span class="wb-code-pill">{hook.action_type}</span></td>
                  <td class="wb-num" title="Total invocations since dispatcher start or last reload">{total}</td>
                  <td class={`wb-num ${hook.failures > 0 ? 'wb-text-danger' : ''}`}>{hook.failures}</td>
                  <td class={`wb-num ${rate > 50 ? 'wb-text-danger' : ''}`}>{rate}%</td>
                  <td><span class={state.badgeClass}>{state.label}</span></td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>

      <div class="wb-mobile-list wb-mobile-only">
        {hooks.map((hook) => {
          const total = hook.successes + hook.failures;
          const rate = errorRatePercent(hook.successes, hook.failures);
          const state = hookState(hook);
          return (
            <button
              type="button"
              class="wb-mobile-card wb-mobile-card--interactive"
              key={hook.name}
              onClick={() => onSelect(hook.name)}
            >
              <div class="wb-mobile-card__header">
                <span class="wb-code-pill">{hook.name}</span>
                <span class={state.badgeClass}>{state.label}</span>
              </div>
              <div class="wb-mobile-card__grid">
                <span>Events</span>
                <strong>{hook.events.length === 0 ? '--' : hook.events.join(', ')}</strong>
                <span>Action</span>
                <strong>{hook.action_type}</strong>
                <span>Calls</span>
                <strong class="wb-num">{total}</strong>
                <span>Error rate</span>
                <strong class={`wb-num ${rate > 50 ? 'wb-text-danger' : ''}`}>{rate}%</strong>
              </div>
            </button>
          );
        })}
      </div>
    </>
  );
}

export default HookList;
