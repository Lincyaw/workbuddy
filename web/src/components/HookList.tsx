import type { HookListEntry } from '../api/hooks';
import { errorRatePercent } from '../api/hooks';
import { HookLatencySparkline } from './HookLatencySparkline';

interface HookListProps {
  hooks: HookListEntry[];
  latencySamples: Record<string, number[]>;
  onSelect: (name: string) => void;
}

export function HookList({ hooks, latencySamples, onSelect }: HookListProps) {
  return (
    <div class="hooks-grid">
      {hooks.map((hook) => {
        const total = hook.successes + hook.failures;
        const rate = errorRatePercent(hook.successes, hook.failures);
        const stateLabel = hook.auto_disabled
          ? 'auto-disabled'
          : hook.enabled
            ? 'ready'
            : 'disabled';
        const badgeClass = hook.auto_disabled
          ? 'wb-badge wb-badge-failed'
          : hook.enabled
            ? 'wb-badge wb-badge-completed'
            : 'wb-badge wb-badge-default';
        return (
          <button
            type="button"
            key={hook.name}
            class="surface-card hook-card"
            onClick={() => onSelect(hook.name)}
          >
            <div class="hook-card-top">
              <div>
                <h2>{hook.name}</h2>
                <p class="mono-copy">{hook.events.join(' · ') || 'manual dispatch only'}</p>
              </div>
              <span class={badgeClass}>{stateLabel}</span>
            </div>
            <div class="hook-card-meta">
              <span class="mono-chip">{hook.action_type}</span>
              <span>{total} calls</span>
              <span>{rate}% error</span>
            </div>
            <div class="hook-card-sparkline">
              <HookLatencySparkline samples={latencySamples[hook.name] || []} />
            </div>
          </button>
        );
      })}
    </div>
  );
}

export default HookList;
