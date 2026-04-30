import { useEffect, useMemo, useState } from 'preact/hooks';
import type { SessionListItem } from '../api/sessions';
import { fetchSessions } from '../api/sessions';

export interface DispatchTickerEntry {
  id: string;
  timestamp: string;
  clock: string;
  kind: string;
  issue: string;
  agent: string;
  workerId: string;
}

function eventKind(session: SessionListItem): string {
  const status = (session.task_status || session.status || '').toLowerCase();
  if (status === 'running' || status === 'in_progress') return 'STATE_ENTRY';
  if (status === 'completed' || status === 'done' || status === 'success') return 'COMPLETION';
  if (status === 'failed' || status === 'error' || status === 'timeout') return 'FAILURE';
  if (status === 'aborted_before_start') return 'FAILED_BEFORE_START';
  if (status === 'queued' || status === 'pending') return 'DISPATCH';
  return status ? status.toUpperCase() : 'SESSION';
}

function clock(ts: string): string {
  const parsed = new Date(ts);
  if (Number.isNaN(parsed.getTime())) return '--:--:--';
  return parsed.toLocaleTimeString([], { hour12: false });
}

export function deriveTickerEntries(sessions: SessionListItem[]): DispatchTickerEntry[] {
  return [...sessions]
    .sort((left, right) => {
      const leftTs = Date.parse(left.finished_at || left.created_at || '') || 0;
      const rightTs = Date.parse(right.finished_at || right.created_at || '') || 0;
      return rightTs - leftTs;
    })
    .slice(0, 10)
    .map((session) => ({
      id: session.session_id,
      timestamp: session.finished_at || session.created_at,
      clock: clock(session.finished_at || session.created_at),
      kind: eventKind(session),
      issue: `#${session.issue_num}`,
      agent: session.agent_name || 'agent',
      workerId: session.worker_id || 'unclaimed',
    }));
}

async function loadTickerEntries(): Promise<DispatchTickerEntry[]> {
  const sessions = await fetchSessions({ limit: 50, offset: 0 });
  return deriveTickerEntries(sessions);
}

export function createDispatchTickerPoller(load: () => Promise<DispatchTickerEntry[]>, apply: (entries: DispatchTickerEntry[]) => void) {
  let inFlight = false;
  return async function poll(): Promise<void> {
    if (inFlight) return;
    inFlight = true;
    try {
      apply(await load());
    } finally {
      inFlight = false;
    }
  };
}

export function DispatchTicker({
  loadEntries = loadTickerEntries,
  intervalMs = 5_000,
}: {
  loadEntries?: () => Promise<DispatchTickerEntry[]>;
  intervalMs?: number;
}) {
  const [entries, setEntries] = useState<DispatchTickerEntry[]>([]);
  const [paused, setPaused] = useState(false);
  const [expanded, setExpanded] = useState(false);

  useEffect(() => {
    let cancelled = false;
    const apply = (next: DispatchTickerEntry[]) => {
      if (!cancelled) setEntries(next);
    };
    const poll = createDispatchTickerPoller(loadEntries, apply);
    void poll();
    const timer = window.setInterval(() => void poll(), intervalMs);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [intervalMs, loadEntries]);

  const marquee = useMemo(() => (entries.length === 0 ? [] : [...entries, ...entries]), [entries]);
  const mobileEntries = useMemo(() => (expanded ? entries : entries.slice(0, 3)), [entries, expanded]);

  return (
    <section
      class="wb-ticker"
      aria-label="Recent dispatch activity"
      data-paused={paused ? 'true' : 'false'}
      tabIndex={0}
      onMouseEnter={() => setPaused(true)}
      onMouseLeave={() => setPaused(false)}
      onFocus={() => setPaused(true)}
      onBlur={() => setPaused(false)}
      onKeyDown={(event) => {
        if (event.key === 'Escape') setPaused(false);
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault();
          setExpanded((value) => !value);
        }
      }}
    >
      <div class="wb-ticker__desktop" data-testid="ticker-track-wrap">
        <div class="wb-ticker__track" data-testid="ticker-track">
          {marquee.length === 0 ? (
            <span class="wb-ticker__item">· --:--:-- · WAITING · no recent session activity ·</span>
          ) : (
            marquee.map((entry, index) => (
              <span class="wb-ticker__item" key={`${entry.id}-${index}`}>
                · {entry.clock} · {entry.kind} · {entry.issue} · {entry.agent} · {entry.workerId}
              </span>
            ))
          )}
        </div>
      </div>
      <button type="button" class="wb-ticker__mobile-toggle" onClick={() => setExpanded((value) => !value)}>
        live dispatch feed {expanded ? 'hide' : 'show'}
      </button>
      <div class="wb-ticker__mobile-list">
        {mobileEntries.map((entry) => (
          <div class="wb-ticker__mobile-row" key={entry.id}>
            <span>{entry.clock}</span>
            <span>{entry.kind}</span>
            <span>{entry.issue}</span>
          </div>
        ))}
      </div>
    </section>
  );
}
