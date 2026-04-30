import { useEffect, useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { Layout } from '../components/Layout';
import { ReloadButton } from '../components/ReloadButton';
import { HookInvocationTimeline } from '../components/HookInvocationTimeline';
import { EmptyState } from '../components/EmptyState';
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
      const entry = list.hooks.find((hook) => hook.name === name) || null;
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
  }, [name]);

  async function handleReload() {
    setState((prev) => ({ ...prev, reloadStatus: 'Reloading hook configuration...' }));
    try {
      const resp = await reloadHooks();
      setState((prev) => ({
        ...prev,
        reloadStatus: `Reloaded ${resp.hook_count} hook(s).`,
      }));
      await load();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'reload failed';
      setState((prev) => ({ ...prev, reloadStatus: `Reload failed: ${msg}` }));
    }
  }

  return (
    <Layout>
      <div class="wb-page-header wb-page-header--split">
        <div>
          <p class="wb-eyebrow">Automation</p>
          <h1 class="wb-page-title wb-inline-title">
            <a href="/hooks" onClick={(e) => { e.preventDefault(); location.route('/hooks'); }}>
              Hooks
            </a>
            <span class="wb-muted"> / </span>
            <code class="wb-code-pill">{name}</code>
          </h1>
          <p class="wb-page-subtitle">Review one hook's runtime state, YAML, and the latest invocation history.</p>
        </div>
        <ReloadButton onConfirm={handleReload} disabled={state.loading} />
      </div>

      {state.reloadStatus && <div class="wb-alert wb-alert--info">{state.reloadStatus}</div>}
      {state.error && <div class="wb-alert wb-alert--danger">{state.error}</div>}
      {state.loading && !state.entry ? (
        <EmptyState icon=".." title="Loading hook detail" copy="Fetching the hook summary, YAML, and invocation timeline." />
      ) : state.entry ? (
        <HookSummary entry={state.entry} />
      ) : null}

      <section class="wb-section">
        <div class="wb-section-heading">
          <div>
            <h2>Configuration</h2>
            <p>The raw hook YAML is kept in a themed code block so long config fits without breaking the layout.</p>
          </div>
        </div>
        <div class="wb-card wb-card--sm">
          {state.config?.yaml ? (
            <pre class="wb-codeblock">{state.config.yaml}</pre>
          ) : state.config?.note ? (
            <div class="wb-muted">{state.config.note}</div>
          ) : (
            <EmptyState icon="{ }" title="No config snapshot available" copy="Reload the dispatcher after adding the hook to capture a YAML snapshot here." />
          )}
        </div>
      </section>

      <section class="wb-section">
        <div class="wb-section-heading">
          <div>
            <h2>Recent invocations</h2>
            <p>Invocation cards align with the rest of the shell and keep stdout and stderr in compact code blocks.</p>
          </div>
        </div>
        <div class="wb-card wb-card--flush">
          {state.invocations ? (
            <HookInvocationTimeline invocations={state.invocations.invocations} />
          ) : (
            <EmptyState icon=".." title="Loading invocations" copy="Reading the most recent calls for this hook." />
          )}
        </div>
      </section>
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
  const badgeClass = entry.auto_disabled
    ? 'wb-badge wb-badge-warning'
    : entry.enabled
    ? 'wb-badge wb-badge-success'
    : 'wb-badge wb-badge-neutral';

  return (
    <div class="wb-card wb-card--md">
      <dl class="wb-key-value-grid">
        <dt>State</dt>
        <dd><span class={badgeClass}>{stateLabel}</span></dd>
        <dt>Events</dt>
        <dd>
          {entry.events.length === 0
            ? '--'
            : entry.events.map((ev) => (
                <span class="wb-event-pill" key={ev}>{ev}</span>
              ))}
        </dd>
        <dt>Action</dt>
        <dd><span class="wb-code-pill">{entry.action_type}</span></dd>
        <dt>Calls</dt>
        <dd class="wb-num">{total} total · {entry.successes} success · {entry.failures} failure · {entry.filtered} filtered · {entry.overflow} overflow</dd>
        <dt>Error rate</dt>
        <dd class="wb-num">{rate}%</dd>
        <dt>Last invoked</dt>
        <dd>{entry.last_invoked_at ? formatTimestamp(entry.last_invoked_at) : <span class="wb-muted">never</span>}</dd>
        <dt>Last error</dt>
        <dd>
          {entry.last_error ? <code class="wb-code-inline wb-text-danger">{entry.last_error}</code> : <span class="wb-muted">none</span>}
        </dd>
      </dl>
    </div>
  );
}

export default HookDetail;
