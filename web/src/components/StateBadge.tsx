const KNOWN_STATES = new Set([
  'developing',
  'reviewing',
  'blocked',
  'queued',
  'done',
  'failed',
]);

function badgeClass(state: string): string {
  switch (state) {
    case 'developing':
      return 'wb-badge wb-badge-warning';
    case 'reviewing':
      return 'wb-badge wb-badge-running';
    case 'blocked':
      return 'wb-badge wb-badge-danger';
    case 'queued':
      return 'wb-badge wb-badge-neutral';
    case 'done':
      return 'wb-badge wb-badge-success';
    case 'failed':
      return 'wb-badge wb-badge-danger';
    default:
      return 'wb-badge wb-badge-neutral';
  }
}

export function StateBadge({ state }: { state: string }) {
  const normalized = (state || '').trim().toLowerCase();
  const label = KNOWN_STATES.has(normalized) ? normalized : normalized || 'unknown';
  return <span class={badgeClass(normalized)}>{label}</span>;
}
