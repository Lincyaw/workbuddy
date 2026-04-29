const KNOWN_STATES = new Set([
  'developing',
  'reviewing',
  'blocked',
  'queued',
  'done',
  'failed',
]);

export function StateBadge({ state }: { state: string }) {
  const normalized = (state || '').trim().toLowerCase();
  const cls = KNOWN_STATES.has(normalized) ? normalized : 'unknown';
  const label = normalized || 'unknown';
  return <span class={`badge ${cls}`}>{label}</span>;
}
