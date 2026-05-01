import { useEffect, useRef, useState } from 'preact/hooks';
import { fetchSessions, type SessionListItem } from '../api/sessions';

export interface DispatchTickerEntry {
  id: string;
  sessionId: string;
  ts: string;
  label: string;
  issue: number;
  agent: string;
  workerId: string;
}

export const TICKER_POLL_MS = 5_000;
const MAX_TICKER_ENTRIES = 10;
const FLASH_MS = 1_800;

function formatClock(ts?: string | null): string {
  if (!ts) return '--:--:--';
  const date = new Date(ts);
  if (Number.isNaN(date.getTime())) return '--:--:--';
  return date.toLocaleTimeString([], {
    hour12: false,
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

function makeEntry(
  session: SessionListItem,
  label: string,
  ts: string,
  suffix: string,
): DispatchTickerEntry {
  return {
    id: `${session.session_id}:${suffix}:${ts}`,
    sessionId: session.session_id,
    ts,
    label,
    issue: session.issue_num,
    agent: session.agent_name || 'agent',
    workerId: session.worker_id || 'worker:unclaimed',
  };
}

function seedEntries(sessions: SessionListItem[]): DispatchTickerEntry[] {
  const seeded = sessions.flatMap((session) => {
    const entries: DispatchTickerEntry[] = [];
    if (session.created_at) {
      entries.push(makeEntry(session, 'DISPATCHED', session.created_at, 'dispatch'));
    }
    if (session.current_state) {
      entries.push(
        makeEntry(
          session,
          `STATE_${session.current_state.toUpperCase()}`,
          session.finished_at || session.created_at,
          'state',
        ),
      );
    }
    if (session.finished_at) {
      const label = (session.status || '').toLowerCase() === 'failed' ? 'FAILED' : 'COMPLETED';
      entries.push(makeEntry(session, label, session.finished_at, 'finished'));
    }
    return entries;
  });
  return mergeTickerEntries([], seeded);
}

function compareSessions(a: SessionListItem, b: SessionListItem): number {
  return (b.created_at || '').localeCompare(a.created_at || '');
}

export function mergeTickerEntries(
  current: DispatchTickerEntry[],
  incoming: DispatchTickerEntry[],
): DispatchTickerEntry[] {
  const seen = new Set<string>();
  const merged = [...incoming, ...current]
    .filter((entry) => {
      if (seen.has(entry.id)) return false;
      seen.add(entry.id);
      return true;
    })
    .sort((a, b) => b.ts.localeCompare(a.ts));
  return merged.slice(0, MAX_TICKER_ENTRIES);
}

export function buildTickerEntries(
  sessions: SessionListItem[],
  previous: Record<string, SessionListItem>,
  currentEntries: DispatchTickerEntry[],
): DispatchTickerEntry[] {
  const nextEntries: DispatchTickerEntry[] = [];
  const ordered = [...sessions].sort(compareSessions);
  if (Object.keys(previous).length === 0 && currentEntries.length === 0) {
    return seedEntries(ordered.slice(0, MAX_TICKER_ENTRIES));
  }
  for (const session of ordered) {
    const prev = previous[session.session_id];
    if (!prev) {
      nextEntries.push(makeEntry(session, 'DISPATCHED', session.created_at, 'dispatch'));
      if (session.current_state) {
        nextEntries.push(
          makeEntry(
            session,
            `STATE_${session.current_state.toUpperCase()}`,
            session.created_at,
            'state',
          ),
        );
      }
      continue;
    }
    if (session.current_state && session.current_state !== prev.current_state) {
      nextEntries.push(
        makeEntry(
          session,
          `STATE_${session.current_state.toUpperCase()}`,
          session.finished_at || session.created_at,
          'state',
        ),
      );
    }
    if ((session.status || '') !== (prev.status || '')) {
      const label = `STATUS_${(session.status || 'unknown').toUpperCase()}`;
      nextEntries.push(
        makeEntry(session, label, session.finished_at || session.created_at, 'status'),
      );
    }
    if (session.finished_at && session.finished_at !== prev.finished_at) {
      const label = (session.status || '').toLowerCase() === 'failed' ? 'FAILED' : 'COMPLETED';
      nextEntries.push(makeEntry(session, label, session.finished_at, 'finished'));
    }
  }
  return mergeTickerEntries(currentEntries, nextEntries);
}

export function DispatchTicker() {
  const [entries, setEntries] = useState<DispatchTickerEntry[]>([]);
  const [degraded, setDegraded] = useState(false);
  const [paused, setPaused] = useState(false);
  const [flashing, setFlashing] = useState(false);
  const previousRef = useRef<Record<string, SessionListItem>>({});
  const inFlightRef = useRef(false);
  const lastTopIdRef = useRef<string | null>(null);
  const flashTimerRef = useRef<number | null>(null);

  useEffect(() => {
    let cancelled = false;

    async function load() {
      if (inFlightRef.current) return;
      inFlightRef.current = true;
      try {
        const sessions = await fetchSessions({ limit: 40 });
        if (cancelled) return;
        setEntries((current) => {
          const next = buildTickerEntries(sessions || [], previousRef.current, current);
          const topId = next[0]?.id ?? null;
          if (topId && lastTopIdRef.current !== null && topId !== lastTopIdRef.current) {
            setFlashing(true);
            if (flashTimerRef.current !== null) {
              window.clearTimeout(flashTimerRef.current);
            }
            flashTimerRef.current = window.setTimeout(() => {
              setFlashing(false);
              flashTimerRef.current = null;
            }, FLASH_MS);
          }
          lastTopIdRef.current = topId;
          return next;
        });
        previousRef.current = Object.fromEntries(
          (sessions || []).map((session) => [session.session_id, session]),
        );
        setDegraded(false);
      } catch {
        if (!cancelled) {
          setDegraded(true);
        }
      } finally {
        inFlightRef.current = false;
      }
    }

    void load();
    const timer = window.setInterval(() => {
      void load();
    }, TICKER_POLL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
      if (flashTimerRef.current !== null) {
        window.clearTimeout(flashTimerRef.current);
      }
    };
  }, []);

  const latest = entries[0];

  return (
    <section
      class={`dispatch-ticker${degraded ? ' dispatch-ticker--degraded' : ''}${paused ? ' is-paused' : ''}${flashing ? ' is-flashing' : ''}`}
      aria-label="Latest dispatch activity"
      onMouseEnter={() => setPaused(true)}
      onMouseLeave={() => setPaused(false)}
    >
      <div class="dispatch-ticker-inner" aria-live="polite">
        {latest ? (
          <span class="dispatch-ticker-entry">
            <span class="dispatch-ticker-dot" aria-hidden="true" />
            {formatClock(latest.ts)}
            <span class="dispatch-ticker-sep">·</span>
            {latest.label}
            <span class="dispatch-ticker-sep">·</span>
            #{latest.issue}
            <span class="dispatch-ticker-sep">·</span>
            {latest.agent}
            <span class="dispatch-ticker-sep">·</span>
            {latest.workerId}
          </span>
        ) : (
          <span class="dispatch-ticker-entry dispatch-ticker-empty">
            waiting for the next dispatch pulse…
          </span>
        )}
      </div>
    </section>
  );
}
