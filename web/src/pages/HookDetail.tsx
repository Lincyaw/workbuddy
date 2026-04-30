import { useEffect, useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { Layout } from '../components/Layout';
import { ReloadButton } from '../components/ReloadButton';
import { HookInvocationTimeline } from '../components/HookInvocationTimeline';
import {
  errorRatePercent,
  fetchHookConfig,
  fetchHookInvocations,
  fetchHooks,
  reloadHooks,
  type HookConfigResponse,
  type HookInvocationsResponse,
  type HookListEntry,
} from '../api/hooks';
import { formatTimestamp } from '../lib/format';

const POLL_INTERVAL_MS = 30_000;

interface DetailState {
  entry: HookListEntry | null;
  invocations: HookInvocationsResponse | null;
  config: HookConfigResponse | null;
  error: string | null;
  loading: boolean;
  reloadStatus: string | null;
}

const INITIAL: DetailState = {
  entry: null,
  invocations: null,
  config: null,
  error: null,
  loading: true,
  reloadStatus: null,
};

export function HookDetail() {
  const { params } = useRoute();
  const location = useLocation();
  const name = decodeURIComponent(params.name || '');
  const [state, setState] = useState<DetailState>(INITIAL);

  async function load() {
    if (!name) {
      setState({ ...INITIAL, error: 'missing hook name', loading: false });
      return;
    }
    try {
      const [list, invs, cfg] = await Promise.all([
        fetchHooks(),
        fetchHookInvocations(name, 20),
        fetchHookConfig(name).catch(() => null),
      ]);
      const entry = list.hooks.find((h) => h.name === name) || null;
      setState((prev) => ({
        ...prev,
        entry,
        invocations: invs,
        config: cfg,
        error: entry ? null : `hook ${name} is not registered`,
        loading: false,
      }));
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'failed to load hook';
      setState((prev) => ({ ...prev, error: msg, loading: false }));
    }
  }

  useEffect(() => {
    void load();
    const timer = setInterval(() => void load(), POLL_INTERVAL_MS);
    return () => clearInterval(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [name]);

  async function handleReload() {
    setState((prev) => ({ ...prev, reloadStatus: 'reloading…' }));
    try {
      const resp = await reloadHooks();
      setState((prev) => ({
        ...prev,
        reloadStatus: `reloaded ${resp.hook_count} hook(s)`,
      }));
      await load();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'reload failed';
      setState((prev) => ({ ...prev, reloadStatus: `reload failed: ${msg}` }));
    }
  }

  return (
    <Layout>
      <div class="wb-hooks-header">
        <h1>
          <a href="/hooks" onClick={(e) => { e.preventDefault(); location.route('/hooks'); }}>
            Hooks
          </a>
          <span class="muted"> / </span>
          <code>{name}</code>
        </h1>
        <ReloadButton onConfirm={handleReload} disabled={state.loading} />
      </div>
      {state.reloadStatus && <div class="wb-hooks-status">{state.reloadStatus}</div>}
      {state.error && <div class="error-banner">{state.error}</div>}
      {state.loading && !state.entry ? (
        <div class="empty">Loading…</div>
      ) : state.entry ? (
        <HookSummary entry={state.entry} />
      ) : null}

      <h2>Configuration</h2>
      <div class="panel" style={{ padding: '0.75rem 1rem' }}>
        {state.config?.yaml ? (
          <pre class="wb-hook-config-yaml">{state.config.yaml}</pre>
        ) : state.config?.note ? (
          <div class="muted">{state.config.note}</div>
        ) : (
          <div class="muted">No config available.</div>
        )}
      </div>

      <h2>Recent invocations</h2>
      <div class="panel" style={{ padding: 0 }}>
        {state.invocations ? (
          <HookInvocationTimeline invocations={state.invocations.invocations} />
        ) : (
          <div class="empty">Loading…</div>
        )}
      </div>
    </Layout>
  );
}

function HookSummary({ entry }: { entry: HookListEntry }) {
  const total = entry.successes + entry.failures;
  const rate = errorRatePercent(entry.successes, entry.failures);
  const stateLabel = entry.auto_disabled
    ? 'auto-disabled (operator must reload)'
    : entry.enabled
    ? 'enabled'
    : 'disabled';
  return (
    <div class="wb-card">
      <dl class="kv">
        <dt>State</dt>
        <dd>
          <span
            class={`badge ${entry.auto_disabled ? 'failed' : entry.enabled ? 'done' : 'queued'}`}
          >
            {stateLabel}
          </span>
        </dd>
        <dt>Events</dt>
        <dd>
          {entry.events.length === 0
            ? '—'
            : entry.events.map((ev) => (
                <span class="wb-event-chip" key={ev}>{ev}</span>
              ))}
        </dd>
        <dt>Action</dt>
        <dd><span class="code-chip">{entry.action_type}</span></dd>
        <dt>Calls</dt>
        <dd>{total} total · {entry.successes} success · {entry.failures} failure · {entry.filtered} filtered · {entry.overflow} overflow</dd>
        <dt>Error rate</dt>
        <dd>{rate}%</dd>
        <dt>Last invoked</dt>
        <dd>{entry.last_invoked_at ? formatTimestamp(entry.last_invoked_at) : <span class="muted">never</span>}</dd>
        <dt>Last error</dt>
        <dd>
          {entry.last_error ? (
            <code class="wb-hook-last-error">{entry.last_error}</code>
          ) : (
            <span class="muted">none</span>
          )}
        </dd>
      </dl>
    </div>
  );
}

export default HookDetail;
