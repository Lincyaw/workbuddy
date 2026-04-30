import type { JSX } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { useRoute } from 'preact-iso';
import { Layout } from '../components/Layout';
import { GitHubIssueLink } from '../components/GitHubIssueLink';
import { splitRepoSlug } from '../utils/github';
import {
  fetchSession,
  fetchSessionEvents,
  sessionStreamURL,
  type SessionDetail as SessionDetailMeta,
  type SessionEvent,
} from '../api/sessions';
import { copyText, formatTimestamp, shortID, statusBadgeClass } from '../lib/format';
import { parseLine, type ParsedMessage, type Runtime } from '../lib/streamMessages';
import { StreamMessageView } from '../components/StreamMessage';
import { mergeSessionEvents } from '../utils/sessionEvents';

const KIND_OPTIONS = ['tool_call', 'tool_result', 'message', 'system'] as const;
type ViewMode = 'pretty' | 'raw';
type KindOption = (typeof KIND_OPTIONS)[number];

const INITIAL_LIMIT = 200;

function classifyKind(kind: string): KindOption | 'other' {
  const k = (kind || '').toLowerCase();
  if (k.includes('tool.call') || k === 'tool_call') return 'tool_call';
  if (k.includes('tool.result') || k === 'tool_result') return 'tool_result';
  if (
    k.includes('message') ||
    k === 'agent.message' ||
    k === 'reasoning' ||
    k === 'agent_message'
  )
    return 'message';
  if (k.includes('system') || k.includes('turn.') || k === 'log' || k.includes('token'))
    return 'system';
  return 'other';
}

function eventClass(kind: string): string {
  const c = classifyKind(kind);
  if (c === 'tool_call') return 'wb-event k-tool_call';
  if (c === 'tool_result') return 'wb-event k-tool_result';
  if (c === 'message') return 'wb-event k-message';
  if (c === 'system') return 'wb-event k-system';
  if ((kind || '').toLowerCase().includes('error')) return 'wb-event k-error';
  return 'wb-event k-default';
}

function shorten(s: string, n = 140): string {
  const flat = String(s || '').replace(/\s+/g, ' ').trim();
  return flat.length > n ? flat.slice(0, n) + '…' : flat;
}

function eventTitle(ev: SessionEvent): string {
  const p = (ev.payload || {}) as Record<string, unknown>;
  const get = (k: string): string => {
    const v = p[k];
    return typeof v === 'string' ? v : '';
  };
  const k = (ev.kind || '').toLowerCase();
  if (k === 'agent.message') return shorten(get('text') || get('content'));
  if (k === 'reasoning') return shorten(get('text') || get('summary'));
  if (k === 'tool.call' || k === 'tool_call') {
    const name = get('name') || get('tool') || 'tool';
    const args = (p.input || p.arguments) as Record<string, unknown> | undefined;
    const keys = args && typeof args === 'object' ? Object.keys(args) : [];
    return `${name}(${keys.join(', ')})`;
  }
  if (k === 'tool.result' || k === 'tool_result') {
    const name = get('name') || get('tool') || 'result';
    return `${name}${p.is_error ? ' · ERROR' : ''}`;
  }
  if (k === 'command.exec') {
    const cmd = p.command;
    return shorten(Array.isArray(cmd) ? cmd.join(' ') : String(cmd || ''));
  }
  if (k === 'command.output') return shorten(get('chunk') || get('output') || get('text'));
  if (k === 'file.change') return `${get('action') || 'change'} ${get('path')}`;
  if (k === 'turn.started') return `turn ${get('turn_id') || ev.turn_id || ''}`;
  if (k === 'turn.completed') {
    const status = get('status') ? ` · ${get('status')}` : '';
    return `turn ${get('turn_id') || ev.turn_id || ''}${status}`;
  }
  if (k === 'token.usage')
    return `${(p.input_tokens as number) || 0} in / ${(p.output_tokens as number) || 0} out`;
  if (k === 'error') return shorten(get('message') || get('error') || 'error');
  if (k === 'log') return shorten(get('message') || get('text'));
  return '';
}

function eventTime(ev: SessionEvent): string {
  if (!ev.ts) return `#${ev.index}`;
  const d = new Date(ev.ts);
  if (Number.isNaN(d.getTime())) return `#${ev.index}`;
  return d.toLocaleTimeString();
}

function defaultExpanded(kind: string): boolean {
  const k = (kind || '').toLowerCase();
  if (k === 'agent.message' || k === 'reasoning' || k.includes('message')) return true;
  if (k === 'error') return true;
  return false;
}

export function SessionDetail() {
  const { params } = useRoute();
  const sessionID = params.id || '';

  const [meta, setMeta] = useState<SessionDetailMeta | null>(null);
  const [metaError, setMetaError] = useState<string | null>(null);
  const [events, setEvents] = useState<SessionEvent[]>([]);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [follow, setFollow] = useState(true);
  const [streamLive, setStreamLive] = useState(false);
  const [pausedOffset, setPausedOffset] = useState<number | null>(null);
  const [search, setSearch] = useState('');
  const [enabledKinds, setEnabledKinds] = useState<Record<KindOption | 'other', boolean>>({
    tool_call: true,
    tool_result: true,
    message: true,
    system: true,
    other: true,
  });
  const [expanded, setExpanded] = useState<Record<number, boolean>>({});
  const [viewMode, setViewMode] = useState<ViewMode>('pretty');

  const lastIndexRef = useRef(-1);
  const esRef = useRef<EventSource | null>(null);
  const seenRef = useRef<Set<number>>(new Set());
  const timelineRef = useRef<HTMLDivElement | null>(null);
  const followRef = useRef(true);

  followRef.current = follow;

  const closeStream = () => {
    if (esRef.current) {
      esRef.current.close();
      esRef.current = null;
    }
    setStreamLive(false);
  };

  // Load meta + initial events.
  useEffect(() => {
    if (!sessionID) return;
    let aborted = false;
    seenRef.current = new Set();
    lastIndexRef.current = -1;
    setMeta(null);
    setMetaError(null);
    setEvents([]);
    setLoadError(null);

    fetchSession(sessionID)
      .then((m) => {
        if (!aborted) setMeta(m);
      })
      .catch((err) => {
        if (!aborted) setMetaError(err?.message || 'failed to load session');
      });

    fetchSessionEvents(sessionID, { tail: true, limit: INITIAL_LIMIT })
      .then((data) => {
        if (aborted) return;
        const items = data.events || [];
        for (const ev of items) {
          if (ev.index > lastIndexRef.current) lastIndexRef.current = ev.index;
        }
        // Default-expand a few kinds; merge into existing map so events the SSE
        // already added before this resolve keep their toggle state.
        setExpanded((prev) => {
          const exp = { ...prev };
          for (const ev of items) {
            if (exp[ev.index] === undefined && defaultExpanded(ev.kind)) {
              exp[ev.index] = true;
            }
          }
          return exp;
        });
        // Functional update + dedupe + sort: the SSE handler may have already
        // pushed events 0..N before this fetch resolves. A plain setEvents(next)
        // would clobber them. See issue #277.
        setEvents((prev) => mergeSessionEvents(prev, items, seenRef.current));
      })
      .catch((err) => {
        if (!aborted) setLoadError(err?.message || 'failed to load events');
      });

    return () => {
      aborted = true;
      closeStream();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessionID]);

  // SSE subscription tied to follow toggle.
  useEffect(() => {
    if (!sessionID) return;
    if (!follow) {
      closeStream();
      return;
    }
    const after = lastIndexRef.current + 1;
    const url = sessionStreamURL(sessionID, after);
    const es = new EventSource(url, { withCredentials: true });
    esRef.current = es;
    setStreamLive(true);
    setPausedOffset(null);
    es.addEventListener('evt', (e: MessageEvent) => {
      try {
        const ev: SessionEvent = JSON.parse(e.data);
        if (seenRef.current.has(ev.index)) return;
        if (ev.index > lastIndexRef.current) lastIndexRef.current = ev.index;
        setEvents((prev) => mergeSessionEvents(prev, [ev], seenRef.current));
        if (followRef.current) {
          requestAnimationFrame(() => {
            const el = timelineRef.current;
            if (el) el.scrollTop = el.scrollHeight;
          });
        }
      } catch {
        /* ignore parse errors */
      }
    });
    es.addEventListener('error', () => {
      setStreamLive(false);
    });
    return () => {
      es.close();
      if (esRef.current === es) {
        esRef.current = null;
      }
    };
  }, [sessionID, follow]);

  function toggleFollow() {
    setFollow((prev) => {
      const next = !prev;
      if (!next) {
        setPausedOffset(lastIndexRef.current);
      } else {
        setPausedOffset(null);
      }
      return next;
    });
  }

  function clearTimeline() {
    closeStream();
    setEvents([]);
    seenRef.current = new Set();
    lastIndexRef.current = -1;
    setPausedOffset(null);
    setExpanded({});
    if (follow) {
      // Re-subscribe immediately with after=0 to pick up the live tail again.
      const url = sessionStreamURL(sessionID, 0);
      const es = new EventSource(url, { withCredentials: true });
      esRef.current = es;
      setStreamLive(true);
      es.addEventListener('evt', (e: MessageEvent) => {
        try {
          const ev: SessionEvent = JSON.parse(e.data);
          if (seenRef.current.has(ev.index)) return;
          if (ev.index > lastIndexRef.current) lastIndexRef.current = ev.index;
          setEvents((prev) => mergeSessionEvents(prev, [ev], seenRef.current));
        } catch {
          /* ignore */
        }
      });
      es.addEventListener('error', () => setStreamLive(false));
    }
  }

  function toggleKind(kind: KindOption | 'other') {
    setEnabledKinds((prev) => ({ ...prev, [kind]: !prev[kind] }));
  }

  function toggleExpanded(idx: number) {
    setExpanded((prev) => ({ ...prev, [idx]: !prev[idx] }));
  }

  const filteredEvents = useMemo(() => {
    const q = search.trim().toLowerCase();
    return events.filter((ev) => {
      const cls = classifyKind(ev.kind);
      if (!enabledKinds[cls]) return false;
      if (!q) return true;
      const kindMatch = (ev.kind || '').toLowerCase().includes(q);
      let payloadMatch = false;
      if (ev.payload) {
        try {
          payloadMatch = JSON.stringify(ev.payload).toLowerCase().includes(q);
        } catch {
          payloadMatch = false;
        }
      }
      return kindMatch || payloadMatch;
    });
  }, [events, enabledKinds, search]);

  if (!sessionID) {
    return (
      <Layout>
        <div class="wb-error">Missing session id in URL.</div>
      </Layout>
    );
  }

  return (
    <Layout>
      <div class="wb-breadcrumb">
        <a href="/">Home</a>
        <span class="sep">/</span>
        <a href="/sessions">Sessions</a>
        <span class="sep">/</span>
        <code>{shortID(sessionID, 16)}</code>
      </div>

      {metaError && <div class="wb-error">Session metadata: {metaError}</div>}

      <div class="wb-card">
        <dl class="wb-meta-grid">
          <dt>Session ID</dt>
          <dd>
            <code>{sessionID}</code>{' '}
            <button
              type="button"
              class="id-copy"
              onClick={() => copyText(sessionID)}
              style={{ marginLeft: 6 }}
            >
              copy
            </button>
          </dd>
          {meta && (
            <>
              <dt>Agent</dt>
              <dd>{meta.agent_name}</dd>
              <dt>Repo</dt>
              <dd>{meta.repo}</dd>
              <dt>Issue</dt>
              <dd>
                #{meta.issue_num}{' '}
                {meta.repo && (
                  <GitHubIssueLink
                    owner={splitRepoSlug(meta.repo).owner}
                    repo={splitRepoSlug(meta.repo).name}
                    num={meta.issue_num}
                    variant="icon"
                  />
                )}
              </dd>
              {meta.worker_id && (
                <>
                  <dt>Worker</dt>
                  <dd>
                    <code>{meta.worker_id}</code>
                  </dd>
                </>
              )}
              <dt>Started</dt>
              <dd>{formatTimestamp(meta.created_at)}</dd>
              <dt>Status</dt>
              <dd>
                <span class={statusBadgeClass(meta.status)}>{meta.status || 'unknown'}</span>
              </dd>
              <dt>Exit code</dt>
              <dd>{meta.exit_code}</dd>
              {meta.task_id && (
                <>
                  <dt>Task ID</dt>
                  <dd>
                    <code>{meta.task_id}</code>
                  </dd>
                </>
              )}
            </>
          )}
        </dl>
      </div>

      <div class="wb-toolbar">
        <strong style={{ fontSize: 13 }}>Timeline</strong>
        <div class="wb-view-toggle" role="tablist" aria-label="View mode">
          <button
            type="button"
            class={viewMode === 'pretty' ? 'active' : ''}
            onClick={() => setViewMode('pretty')}
            aria-pressed={viewMode === 'pretty'}
          >
            Pretty
          </button>
          <button
            type="button"
            class={viewMode === 'raw' ? 'active' : ''}
            onClick={() => setViewMode('raw')}
            aria-pressed={viewMode === 'raw'}
          >
            Raw
          </button>
        </div>
        <div class="kinds">
          {(['tool_call', 'tool_result', 'message', 'system'] as const).map((k) => (
            <label key={k} class={enabledKinds[k] ? 'checked' : ''}>
              <input
                type="checkbox"
                checked={enabledKinds[k]}
                onChange={() => toggleKind(k)}
              />
              {k}
            </label>
          ))}
        </div>
        <input
          type="search"
          placeholder="Filter…"
          value={search}
          onInput={(e) => setSearch((e.target as HTMLInputElement).value)}
          style={{ minWidth: 160 }}
        />
        <button
          type="button"
          class={follow ? 'active' : ''}
          onClick={toggleFollow}
          title="Toggle SSE tail-follow"
        >
          {follow ? 'Tail follow' : 'Paused'}
        </button>
        <button type="button" onClick={clearTimeline}>Clear</button>
        <span class="grow" />
        <span class={`wb-status${streamLive ? ' live' : ''}`}>
          {streamLive ? 'streaming' : follow ? 'connecting…' : 'paused'}
        </span>
      </div>

      {loadError && <div class="wb-error">Events: {loadError}</div>}

      <div class={`wb-timeline${viewMode === 'pretty' ? ' wb-timeline-pretty' : ''}`} ref={timelineRef}>
        {filteredEvents.length === 0 ? (
          <div class="wb-empty">
            {events.length === 0 ? 'No events yet.' : 'No events match the filters.'}
          </div>
        ) : viewMode === 'raw' ? (
          filteredEvents.map((ev) => (
            <Event
              key={ev.index}
              ev={ev}
              expanded={expanded[ev.index] ?? defaultExpanded(ev.kind)}
              onToggle={() => toggleExpanded(ev.index)}
            />
          ))
        ) : (
          renderPretty(filteredEvents, (meta?.runtime as Runtime | undefined) ?? 'unknown', expanded, toggleExpanded)
        )}
      </div>

      <div class={`wb-bottom-bar${streamLive ? ' live' : ''}`}>
        <span>
          {events.length} events
          {streamLive ? ' · live' : pausedOffset != null ? ` · paused at offset ${pausedOffset}` : ''}
        </span>
        <span>{filteredEvents.length} shown</span>
      </div>
    </Layout>
  );
}

function Event({
  ev,
  expanded,
  onToggle,
}: {
  ev: SessionEvent;
  expanded: boolean;
  onToggle: () => void;
}) {
  const payloadText = ev.payload == null ? '' : JSON.stringify(ev.payload, null, 2);
  return (
    <div class={eventClass(ev.kind)}>
      <div class="wb-event-header" onClick={onToggle}>
        <span class="wb-kind-badge">{ev.kind || '?'}</span>
        <span class="wb-event-title">{eventTitle(ev) || '(no summary)'}</span>
        <span class="wb-event-time">{eventTime(ev)}</span>
      </div>
      {expanded && (
        <div class="wb-event-body">
          <pre>{payloadText || '(no payload)'}</pre>
          {ev.truncated && (
            <div class="wb-event-truncated">payload truncated for transport</div>
          )}
        </div>
      )}
    </div>
  );
}

function renderPretty(
  events: SessionEvent[],
  runtime: Runtime,
  expanded: Record<number, boolean>,
  toggleExpanded: (idx: number) => void,
): JSX.Element[] {
  // Group consecutive events that share a turn_id.
  const groups: { turnId: string; events: SessionEvent[] }[] = [];
  for (const ev of events) {
    const tid = ev.turn_id || '';
    const last = groups[groups.length - 1];
    if (!last || last.turnId !== tid) {
      groups.push({ turnId: tid, events: [ev] });
    } else {
      last.events.push(ev);
    }
  }
  return groups.map((g, gi) => (
    <div key={`turn-${gi}-${g.turnId}`} class="wb-turn-group">
      <div class="wb-turn-boundary">
        <span class="wb-turn-label">turn{g.turnId ? ` · ${shortID(g.turnId, 12)}` : ''}</span>
      </div>
      <div class="wb-turn-body">
        {g.events.map((ev) => (
          <PrettyEvent
            key={ev.index}
            ev={ev}
            runtime={runtime}
            expanded={expanded[ev.index] ?? defaultExpanded(ev.kind)}
            onToggle={() => toggleExpanded(ev.index)}
          />
        ))}
      </div>
    </div>
  ));
}

function PrettyEvent({
  ev,
  runtime,
  expanded,
  onToggle,
}: {
  ev: SessionEvent;
  runtime: Runtime;
  expanded: boolean;
  onToggle: () => void;
}) {
  // Only `kind=log` events with a JSON `payload.line` get the structured
  // renderer; all other event kinds keep the existing Event view (which
  // already renders the typed launcher payload nicely).
  if ((ev.kind || '').toLowerCase() === 'log') {
    const line = extractLine(ev.payload);
    if (line) {
      const parsed: ParsedMessage = parseLine(line, { runtime });
      return (
        <div class={`wb-event k-pretty pretty-${parsed.kind}`}>
          <div class="wb-event-time-row">
            <span class="wb-event-time">{eventTime(ev)}</span>
          </div>
          <StreamMessageView msg={parsed} />
        </div>
      );
    }
  }
  return <Event ev={ev} expanded={expanded} onToggle={onToggle} />;
}

function extractLine(payload: unknown): string | null {
  if (!payload || typeof payload !== 'object') return null;
  const p = payload as Record<string, unknown>;
  const line = p.line;
  return typeof line === 'string' ? line : null;
}

export default SessionDetail;
