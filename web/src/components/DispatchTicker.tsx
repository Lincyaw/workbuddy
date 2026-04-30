import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
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
const MOBILE_VISIBLE_ENTRIES = 3;

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
  const [expanded, setExpanded] = useState(false);
  const previousRef = useRef<Record<string, SessionListItem>>({});
  const inFlightRef = useRef(false);

  useEffect(() => {
    let cancelled = false;

    async function load() {
      if (inFlightRef.current) return;
      inFlightRef.current = true;
      try {
        const sessions = await fetchSessions({ limit: 40 });
        if (cancelled) return;
        setEntries((current) =>
          buildTickerEntries(sessions || [], previousRef.current, current),
        );
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
    };
  }, []);

  const marqueeEntries = useMemo(() => [...entries, ...entries], [entries]);
  const mobileEntries = entries.slice(0, MOBILE_VISIBLE_ENTRIES);

  return (
    <section
      class={`dispatch-ticker${degraded ? ' dispatch-ticker--degraded' : ''}${paused ? ' is-paused' : ''}${expanded ? ' is-expanded' : ''}`}
      aria-label="Recent dispatch activity"
      tabIndex={0}
      onMouseEnter={() => setPaused(true)}
      onMouseLeave={() => setPaused(false)}
      onFocusCapture={() => setPaused(true)}
      onBlurCapture={(event) => {
        const next = event.relatedTarget as Node | null;
        if (!next || !event.currentTarget.contains(next)) {
          setPaused(false);
        }
      }}
      onKeyDown={(event) => {
        if (event.key === 'Escape') {
          setPaused(false);
          (event.currentTarget as HTMLElement).blur();
        }
      }}
    >
      <div class="dispatch-ticker-inner dispatch-ticker-desktop" aria-live="polite">
        <div class="dispatch-ticker-track">
          {marqueeEntries.length > 0 ? (
            marqueeEntries.map((entry, index) => (
              <span class="dispatch-ticker-entry" key={`${entry.id}:${index}`}>
                <span class="dispatch-ticker-dot" aria-hidden="true" />
                {formatClock(entry.ts)}
                <span class="dispatch-ticker-sep">·</span>
                {entry.label}
                <span class="dispatch-ticker-sep">·</span>
                #{entry.issue}
                <span class="dispatch-ticker-sep">·</span>
                {entry.agent}
                <span class="dispatch-ticker-sep">·</span>
                {entry.workerId}
              </span>
            ))
          ) : (
            <span class="dispatch-ticker-entry">waiting for the next dispatch pulse…</span>
          )}
        </div>
      </div>

      <button
        type="button"
        class="dispatch-ticker-mobile"
        aria-expanded={expanded}
        onClick={() => setExpanded((value) => !value)}
      >
        <span class="dispatch-ticker-mobile-label">dispatch feed</span>
        <span class="dispatch-ticker-mobile-summary">
          {mobileEntries.length > 0
            ? mobileEntries
                .map((entry) => `${formatClock(entry.ts)} ${entry.label} #${entry.issue}`)
                .join(' / ')
            : 'waiting for the next dispatch pulse…'}
        </span>
      </button>

      {expanded ? (
        <div class="dispatch-ticker-mobile-list">
          {entries.map((entry) => (
            <div class="dispatch-ticker-mobile-row" key={entry.id}>
              <strong>{formatClock(entry.ts)}</strong>
              <span>{entry.label}</span>
              <span>#{entry.issue}</span>
              <span>{entry.agent}</span>
              <span>{entry.workerId}</span>
            </div>
          ))}
        </div>
      ) : null}
    </section>
  );
}
