// formatRelative renders a duration ago in compact form ("5m ago", "2h ago").
// Returns "—" when ts is missing or unparseable so the dashboard doesn't show
// a confusing "NaN ago" cell.
export function formatRelative(ts: string | null | undefined, now: Date = new Date()): string {
  if (!ts) return '—';
  const parsed = new Date(ts);
  if (Number.isNaN(parsed.getTime())) return '—';
  const seconds = Math.max(0, Math.floor((now.getTime() - parsed.getTime()) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  return `${days}d ago`;
}

export const ONE_HOUR_SECONDS = 3600;
