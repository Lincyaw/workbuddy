import type { JSX } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { useRoute } from 'preact-iso';
import { Layout } from '../components/Layout';
import { GitHubIssueLink } from '../components/GitHubIssueLink';
import { DegradedSessionCard } from '../components/DegradedSessionCard';
import { EmptyState } from '../components/EmptyState';
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
  const value = (kind || '').toLowerCase();
  if (value.includes('tool.call') || value === 'tool_call') return 'tool_call';
  if (value.includes('tool.result') || value === 'tool_result') return 'tool_result';
  if (value.includes('message') || value === 'agent.message' || value === 'reasoning' || value === 'agent_message') return 'message';
  if (value.includes('system') || value.includes('turn.') || value === 'log' || value.includes('token')) return 'system';
  return 'other';
}

function eventClass(kind: string): string {
  const value = classifyKind(kind);
  if (value === 'tool_call') return 'wb-event k-tool_call';
  if (value === 'tool_result') return 'wb-event k-tool_result';
  if (value === 'message') return 'wb-event k-message';
  if (value === 'system') return 'wb-event k-system';
  if ((kind || '').toLowerCase().includes('error')) return 'wb-event k-error';
  return 'wb-event k-default';
}

function kindGlyph(kind: string): string {
  const value = classifyKind(kind);
  if (value === 'tool_call') return 'TC';
  if (value === 'tool_result') return 'TR';
  if (value === 'message') return 'MSG';
  if (value === 'system') return 'SYS';
  if ((kind || '').toLowerCase().includes('error')) return 'ERR';
  return 'EVT';
}

function shorten(value: string, length = 140): string {
  const flat = String(value || '').replace(/\s+/g, ' ').trim();
  return flat.length > length ? flat.slice(0, length) + '...' : flat;
}

function eventTitle(ev: SessionEvent): string {
  const payload = (ev.payload || {}) as Record<string, unknown>;
  const get = (key: string): string => {
    const value = payload[key];
    return typeof value === 'string' ? value : '';
  };
  const kind = (ev.kind || '').toLowerCase();
  if (kind === 'agent.message') return shorten(get('text') || get('content'));
  if (kind === 'reasoning') return shorten(get('text') || get('summary'));
  if (kind === 'tool.call' || kind === 'tool_call') {
    const name = get('name') || get('tool') || 'tool';
    const args = (payload.input || payload.arguments) as Record<string, unknown> | undefined;
    const keys = args && typeof args === 'object' ? Object.keys(args) : [];
    return `${name}(${keys.join(', ')})`;
  }
  if (kind === 'tool.result' || kind === 'tool_result') {
    const name = get('name') || get('tool') || 'result';
    return `${name}${payload.is_error ? ' · ERROR' : ''}`;
  }
  if (kind === 'command.exec') {
    const command = payload.command;
    return shorten(Array.isArray(command) ? command.join(' ') : String(command || ''));
  }
  if (kind === 'command.output') return shorten(get('chunk') || get('output') || get('text'));
  if (kind === 'file.change') return `${get('action') || 'change'} ${get('path')}`;
  if (kind === 'turn.started') return `turn ${get('turn_id') || ev.turn_id || ''}`;
  if (kind === 'turn.completed') {
    const status = get('status') ? ` · ${get('status')}` : '';
    return `turn ${get('turn_id') || ev.turn_id || ''}${status}`;
  }
  if (kind === 'token.usage') return `${(payload.input_tokens as number) || 0} in / ${(payload.output_tokens as number) || 0} out`;
  if (kind === 'error') return shorten(get('message') || get('error') || 'error');
  if (kind === 'log') return shorten(get('message') || get('text'));
  return '';
}

function eventTime(ev: SessionEvent): string {
  if (!ev.ts) return `#${ev.index}`;
  const date = new Date(ev.ts);
  if (Number.isNaN(date.getTime())) return `#${ev.index}`;
  return date.toLocaleTimeString();
}

function defaultExpanded(kind: string): boolean {
  const value = (kind || '').toLowerCase();
  if (value === 'agent.message' || value === 'reasoning' || value.includes('message')) return true;
  if (value === 'error') return true;
  return false;
}

export function SessionDetail() {
  const { params } = useRoute();
  const sessionID = params.id || '';

  const [meta, setMeta] = useState<SessionDetailMeta | null>(null);
  const [metaError, setMetaError] = useState<string | null>(null);
  const [events, setEvents] = useState<SessionEvent[]>([]);
  const [eventsTotal, setEventsTotal] = useState<number | null>(null);
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
      .then((detail) => {
        if (!aborted) setMeta(detail);
      })
      .catch((err) => {
        if (!aborted) setMetaError(err?.message || 'failed to load session');
      });

    fetchSessionEvents(sessionID, { tail: true, limit: INITIAL_LIMIT })
      .then((data) => {
        if (aborted) return;
        const items = data.events || [];
        for (const event of items) {
          if (event.index > lastIndexRef.current) lastIndexRef.current = event.index;
        }
        setExpanded((prev) => {
          const next = { ...prev };
          for (const event of items) {
            if (next[event.index] === undefined && defaultExpanded(event.kind)) {
              next[event.index] = true;
            }
          }
          return next;
        });
        setEvents((prev) => mergeSessionEvents(prev, items, seenRef.current));
        setEventsTotal(typeof data.total === 'number' ? data.total : 0);
      })
      .catch((err) => {
        if (!aborted) setLoadError(err?.message || 'failed to load events');
      });

    return () => {
      aborted = true;
      closeStream();
    };
  }, [sessionID]);

  useEffect(() => {
    if (!sessionID) return;
    if (!follow) {
      closeStream();
      return;
    }
    const after = lastIndexRef.current + 1;
    const es = new EventSource(sessionStreamURL(sessionID, after), { withCredentials: true });
    esRef.current = es;
    setStreamLive(true);
    setPausedOffset(null);

    es.addEventListener('evt', (evt: MessageEvent) => {
      try {
        const event: SessionEvent = JSON.parse(evt.data);
        if (seenRef.current.has(event.index)) return;
        if (event.index > lastIndexRef.current) lastIndexRef.current = event.index;
        setEvents((prev) => mergeSessionEvents(prev, [event], seenRef.current));
        if (followRef.current) {
          requestAnimationFrame(() => {
            const node = timelineRef.current;
            if (node) node.scrollTop = node.scrollHeight;
          });
        }
      } catch {
        // ignore parse failures from partial or malformed stream lines
      }
    });
    es.addEventListener('error', () => {
      setStreamLive(false);
    });
    return () => {
      es.close();
      if (esRef.current === es) esRef.current = null;
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
      const es = new EventSource(sessionStreamURL(sessionID, 0), { withCredentials: true });
      esRef.current = es;
      setStreamLive(true);
      es.addEventListener('evt', (evt: MessageEvent) => {
        try {
          const event: SessionEvent = JSON.parse(evt.data);
          if (seenRef.current.has(event.index)) return;
          if (event.index > lastIndexRef.current) lastIndexRef.current = event.index;
          setEvents((prev) => mergeSessionEvents(prev, [event], seenRef.current));
        } catch {
          // ignore
        }
      });
      es.addEventListener('error', () => setStreamLive(false));
    }
  }

  function toggleKind(kind: KindOption | 'other') {
    setEnabledKinds((prev) => ({ ...prev, [kind]: !prev[kind] }));
  }

  function toggleExpanded(index: number) {
    setExpanded((prev) => ({ ...prev, [index]: !prev[index] }));
  }

  const filteredEvents = useMemo(() => {
    const query = search.trim().toLowerCase();
    return events.filter((event) => {
      const kind = classifyKind(event.kind);
      if (!enabledKinds[kind]) return false;
      if (!query) return true;
      const kindMatch = (event.kind || '').toLowerCase().includes(query);
      let payloadMatch = false;
      if (event.payload) {
        try {
          payloadMatch = JSON.stringify(event.payload).toLowerCase().includes(query);
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
        <div class="wb-alert wb-alert--danger">Missing session id in URL.</div>
      </Layout>
    );
  }

  return (
    <Layout>
      <div class="wb-breadcrumb">
        <a href="/">Home</a>
        <span class="wb-breadcrumb__sep">/</span>
        <a href="/sessions">Sessions</a>
        <span class="wb-breadcrumb__sep">/</span>
        <code class="wb-code-pill">{shortID(sessionID, 16)}</code>
      </div>

      {metaError && <div class="wb-alert wb-alert--danger">Session metadata: {metaError}</div>}

      <DegradedSessionCard meta={meta} eventsTotal={eventsTotal} />

      <div class="wb-card wb-card--md">
        <dl class="wb-meta-grid">
          <dt>Session ID</dt>
          <dd>
            <code class="wb-code-inline">{sessionID}</code>{' '}
            <button type="button" class="wb-button wb-button--ghost wb-button--mini id-copy" onClick={() => copyText(sessionID)}>
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
                  <dd><code class="wb-code-inline">{meta.worker_id}</code></dd>
                </>
              )}
              <dt>Started</dt>
              <dd class="wb-time">{formatTimestamp(meta.created_at)}</dd>
              <dt>Status</dt>
              <dd><span class={statusBadgeClass(meta.status)}>{meta.status || 'unknown'}</span></dd>
              <dt>Exit code</dt>
              <dd class="wb-num">{meta.exit_code}</dd>
              {meta.task_id && (
                <>
                  <dt>Task ID</dt>
                  <dd><code class="wb-code-inline">{meta.task_id}</code></dd>
                </>
              )}
            </>
          )}
        </dl>
      </div>

      <div class="wb-toolbar-grid">
        <div class="wb-toolbar-grid__label">Timeline</div>
        <div class="wb-view-toggle" role="tablist" aria-label="View mode">
          <button type="button" class={viewMode === 'pretty' ? 'active' : ''} onClick={() => setViewMode('pretty')} aria-pressed={viewMode === 'pretty'}>
            Pretty
          </button>
          <button type="button" class={viewMode === 'raw' ? 'active' : ''} onClick={() => setViewMode('raw')} aria-pressed={viewMode === 'raw'}>
            Raw
          </button>
        </div>
        <div class="wb-kind-filters">
          {KIND_OPTIONS.map((kind) => (
            <button
              key={kind}
              type="button"
              class={`wb-filter-chip${enabledKinds[kind] ? ' is-active' : ''}`}
              onClick={() => toggleKind(kind)}
              aria-pressed={enabledKinds[kind]}
            >
              {kind.replace('_', ' ')}
            </button>
          ))}
        </div>
        <div class="wb-toolbar-grid__search">
          <input type="search" placeholder="Filter events" value={search} onInput={(e) => setSearch((e.target as HTMLInputElement).value)} />
        </div>
        <button type="button" class={`wb-live-toggle${follow ? ' is-active' : ''}`} onClick={toggleFollow} title="Toggle SSE tail-follow">
          <span class={`wb-live-dot${streamLive ? ' is-live' : ''}`} aria-hidden="true" />
          {follow ? 'Live follow' : 'Paused'}
        </button>
        <div class="wb-toolbar-grid__actions">
          <button type="button" class="wb-button wb-button--ghost" onClick={clearTimeline}>Clear</button>
          <span class={`wb-live-state${streamLive ? ' is-live' : ''}`}>{streamLive ? 'streaming' : follow ? 'connecting...' : 'paused'}</span>
        </div>
      </div>

      {loadError && <div class="wb-alert wb-alert--danger">Events: {loadError}</div>}

      <div class={`wb-timeline${viewMode === 'pretty' ? ' wb-timeline-pretty' : ''}`} ref={timelineRef}>
        {filteredEvents.length === 0 ? (
          <EmptyState
            icon="[]"
            title={events.length === 0 ? 'No events yet' : 'No events match these filters'}
            copy={events.length === 0 ? 'The session exists, but no event payloads have landed yet.' : 'Relax the event kind chips or clear the text filter to bring rows back.'}
          />
        ) : viewMode === 'raw' ? (
          filteredEvents.map((event) => (
            <Event key={event.index} ev={event} expanded={expanded[event.index] ?? defaultExpanded(event.kind)} onToggle={() => toggleExpanded(event.index)} />
          ))
        ) : (
          renderPretty(filteredEvents, (meta?.runtime as Runtime | undefined) ?? 'unknown', expanded, toggleExpanded)
        )}
      </div>

      <div class={`wb-bottom-bar${streamLive ? ' is-live' : ''}`}>
        <span class="wb-num">
          {events.length} events
          {streamLive ? ' · live' : pausedOffset != null ? ` · paused at offset ${pausedOffset}` : ''}
        </span>
        <span class="wb-num">{filteredEvents.length} shown</span>
      </div>
    </Layout>
  );
}

function Event({ ev, expanded, onToggle }: { ev: SessionEvent; expanded: boolean; onToggle: () => void; }) {
  const payloadText = ev.payload == null ? '' : JSON.stringify(ev.payload, null, 2);
  return (
    <div class={eventClass(ev.kind)}>
      <button type="button" class="wb-event-header" onClick={onToggle} aria-expanded={expanded}>
        <span class="wb-kind-badge"><span class="wb-kind-glyph">{kindGlyph(ev.kind)}</span>{ev.kind || '?'}</span>
        <span class="wb-event-title">{eventTitle(ev) || '(no summary)'}</span>
        <span class="wb-event-time">{eventTime(ev)}</span>
      </button>
      {expanded && (
        <div class="wb-event-body">
          <pre class="wb-codeblock">{payloadText || '(no payload)'}</pre>
          {ev.truncated && <div class="wb-event-truncated">payload truncated for transport</div>}
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
  const groups: { turnId: string; events: SessionEvent[] }[] = [];
  for (const event of events) {
    const turnId = event.turn_id || '';
    const last = groups[groups.length - 1];
    if (!last || last.turnId !== turnId) {
      groups.push({ turnId, events: [event] });
    } else {
      last.events.push(event);
    }
  }
  return groups.map((group, groupIndex) => (
    <div key={`turn-${groupIndex}-${group.turnId}`} class="wb-turn-group">
      <div class="wb-turn-boundary">
        <span class="wb-turn-label">turn{group.turnId ? ` · ${shortID(group.turnId, 12)}` : ''}</span>
      </div>
      <div class="wb-turn-body">
        {group.events.map((event) => (
          <PrettyEvent
            key={event.index}
            ev={event}
            runtime={runtime}
            expanded={expanded[event.index] ?? defaultExpanded(event.kind)}
            onToggle={() => toggleExpanded(event.index)}
          />
        ))}
      </div>
    </div>
  ));
}

function PrettyEvent({ ev, runtime, expanded, onToggle }: { ev: SessionEvent; runtime: Runtime; expanded: boolean; onToggle: () => void; }) {
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
  const line = (payload as Record<string, unknown>).line;
  return typeof line === 'string' ? line : null;
}

export default SessionDetail;
