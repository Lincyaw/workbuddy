export function shortID(id: string, n = 16): string {
  if (id.length <= n) return id;
  return id.slice(0, n);
}

export function formatTimestamp(ts?: string | null, withSeconds = false): string {
  if (!ts) return '';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  const yyyy = d.getFullYear();
  const mm = String(d.getMonth() + 1).padStart(2, '0');
  const dd = String(d.getDate()).padStart(2, '0');
  const hh = String(d.getHours()).padStart(2, '0');
  const mi = String(d.getMinutes()).padStart(2, '0');
  const ss = String(d.getSeconds()).padStart(2, '0');
  return withSeconds
    ? `${yyyy}-${mm}-${dd} ${hh}:${mi}:${ss}`
    : `${yyyy}-${mm}-${dd} ${hh}:${mi}`;
}

export function formatClock(ts?: string | null): string {
  if (!ts) return '--:--:--';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return '--:--:--';
  const hh = String(d.getHours()).padStart(2, '0');
  const mi = String(d.getMinutes()).padStart(2, '0');
  const ss = String(d.getSeconds()).padStart(2, '0');
  return `${hh}:${mi}:${ss}`;
}

export function formatDuration(seconds?: number | null): string {
  if (seconds == null || Number.isNaN(seconds)) return '—';
  if (seconds < 60) return `${Math.max(0, Math.round(seconds))}s`;
  const mins = Math.floor(seconds / 60);
  const secs = Math.round(seconds % 60);
  if (mins < 60) return `${mins}m ${String(secs).padStart(2, '0')}s`;
  const hours = Math.floor(mins / 60);
  const remMins = mins % 60;
  return `${hours}h ${String(remMins).padStart(2, '0')}m`;
}

export function isToday(ts?: string | null): boolean {
  if (!ts) return false;
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return false;
  const now = new Date();
  return (
    d.getFullYear() === now.getFullYear() &&
    d.getMonth() === now.getMonth() &&
    d.getDate() === now.getDate()
  );
}

export function statusBadgeClass(status?: string): string {
  const key = (status || '').toLowerCase();
  switch (key) {
    case 'running':
    case 'in_progress':
      return 'wb-badge wb-badge-running';
    case 'completed':
    case 'done':
    case 'success':
      return 'wb-badge wb-badge-completed';
    case 'failed':
    case 'error':
    case 'timeout':
    case 'aborted_before_start':
      return 'wb-badge wb-badge-failed';
    case 'pending':
    case 'queued':
      return 'wb-badge wb-badge-pending';
    default:
      return 'wb-badge wb-badge-default';
  }
}

export function copyText(value: string): void {
  if (navigator.clipboard?.writeText) {
    navigator.clipboard.writeText(value).catch(() => {});
    return;
  }
  const ta = document.createElement('textarea');
  ta.value = value;
  document.body.appendChild(ta);
  ta.select();
  try {
    document.execCommand('copy');
  } catch {
    /* ignore */
  }
  document.body.removeChild(ta);
}
