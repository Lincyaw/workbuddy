import { useEffect, useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
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
import { EmptyState } from '../components/EmptyState';
import { HookInvocationTimeline } from '../components/HookInvocationTimeline';
import { Layout } from '../components/Layout';
import { ReloadButton } from '../components/ReloadButton';
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

function stateTone(entry: HookListEntry): 'success' | 'neutral' | 'danger' {
  if (entry.auto_disabled) return 'danger';
  if (entry.enabled) return 'success';
  return 'neutral';
}

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
      const [list, invocations, config] = await Promise.all([
        fetchHooks(),
        fetchHookInvocations(name, 20),
        fetchHookConfig(name).catch(() => null),
      ]);
      const entry = list.hooks.find((hook) => hook.name === name) || null;
      setState((current) => ({
        ...current,
        entry,
        invocations,
        config,
        error: entry ? null : `hook ${name} is not registered`,
        loading: false,
      }));
    } catch (error) {
      setState((current) => ({ ...current, loading: false, error: error instanceof Error ? error.message : 'failed to load hook' }));
    }
  }

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [name]);

  async function handleReload() {
    setState((current) => ({ ...current, reloadStatus: 'reloading hook configuration...' }));
    try {
      const response = await reloadHooks();
      setState((current) => ({ ...current, reloadStatus: `reloaded ${response.hook_count} hook(s)` }));
      await load();
    } catch (error) {
      setState((current) => ({ ...current, reloadStatus: `reload failed: ${error instanceof Error ? error.message : 'unknown error'}` }));
    }
  }

  return (
    <Layout>
      <section class="wb-stack">
        <header class="flex flex-wrap items-end justify-between gap-3">
          <div>
            <a href="/hooks" class="wb-backlink" onClick={(event) => { event.preventDefault(); location.route('/hooks'); }}>
              ← hooks
            </a>
            <p class="wb-section-label">hook detail</p>
            <h1 class="wb-page-title">{name}</h1>
            <p class="wb-page-copy">YAML, runtime counters, and the invocation rail share the same event recipe as sessions.</p>
          </div>
          <ReloadButton onConfirm={handleReload} disabled={state.loading} />
        </header>

        {state.reloadStatus ? <div class="wb-panel">{state.reloadStatus}</div> : null}
        {state.error ? <div class="wb-panel text-state-danger">{state.error}</div> : null}

        {state.loading && !state.entry ? (
          <EmptyState glyph="loading" title="loading hook detail" copy="Fetching the hook summary, YAML, and invocation rail." />
        ) : state.entry ? (
          <section class="wb-detail-grid">
            <aside class="wb-panel wb-detail-sidebar">
              <dl class="wb-kv">
                <dt>state</dt>
                <dd><span class={`wb-badge wb-badge--${stateTone(state.entry)}`}>{state.entry.auto_disabled ? 'auto-disabled' : state.entry.enabled ? 'armed' : 'disabled'}</span></dd>
                <dt>events</dt>
                <dd><span class="wb-id-pill">{state.entry.events.join(', ') || '*'}</span></dd>
                <dt>action</dt>
                <dd><span class="wb-id-pill">{state.entry.action_type}</span></dd>
                <dt>calls</dt>
                <dd class="wb-num">{state.entry.successes + state.entry.failures}</dd>
                <dt>error rate</dt>
                <dd class="wb-num">{errorRatePercent(state.entry.successes, state.entry.failures)}%</dd>
                <dt>last invoked</dt>
                <dd class="wb-time">{state.entry.last_invoked_at ? formatTimestamp(state.entry.last_invoked_at) : '--'}</dd>
              </dl>
            </aside>

            <div class="wb-stack">
              <section class="wb-panel">
                <p class="wb-section-label">configuration</p>
                {state.config?.yaml ? (
                  <pre class="wb-codeblock">{state.config.yaml}</pre>
                ) : (
                  <EmptyState glyph="hooks" title="no config snapshot available" copy="Reload the dispatcher after adding the hook to capture a YAML snapshot here." />
                )}
              </section>

              <section class="wb-table-shell">
                <div class="border-b border-border-hairline px-4 py-3">
                  <p class="wb-section-label mb-1">invocations</p>
                  <h2 class="m-0 text-[20px]">recent hook executions</h2>
                </div>
                <HookInvocationTimeline invocations={state.invocations?.invocations || []} />
              </section>
            </div>
          </section>
        ) : null}
      </section>
    </Layout>
  );
}

export default HookDetail;
