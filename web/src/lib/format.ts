export function shortID(id: string, n = 16): string {
  if (id.length <= n) return id;
  return id.slice(0, n);
}

export function formatTimestamp(ts?: string | null): string {
  if (!ts) return '';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  const yyyy = d.getFullYear();
  const mm = String(d.getMonth() + 1).padStart(2, '0');
  const dd = String(d.getDate()).padStart(2, '0');
  const hh = String(d.getHours()).padStart(2, '0');
  const mi = String(d.getMinutes()).padStart(2, '0');
  return `${yyyy}-${mm}-${dd} ${hh}:${mi}`;
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
      return 'wb-badge wb-badge-success';
    case 'failed':
    case 'error':
    case 'timeout':
      return 'wb-badge wb-badge-danger';
    case 'aborted_before_start':
    case 'degraded':
      return 'wb-badge wb-badge-warning';
    case 'pending':
    case 'queued':
    case 'disabled':
      return 'wb-badge wb-badge-neutral';
    default:
      return 'wb-badge wb-badge-neutral';
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
