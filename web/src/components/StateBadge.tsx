function stateTone(state: string): string {
  switch (state) {
    case 'developing':
    case 'running':
    case 'reviewing':
      return 'running';
    case 'done':
    case 'completed':
    case 'success':
      return 'success';
    case 'blocked':
    case 'failed':
    case 'error':
      return 'danger';
    case 'queued':
    case 'pending':
      return 'neutral';
    default:
      return 'warning';
  }
}

export function StateBadge({ state }: { state: string }) {
  const normalized = (state || 'unknown').trim().toLowerCase() || 'unknown';
  return <span class={`wb-badge wb-badge--${stateTone(normalized)}`}>{normalized}</span>;
}
