import { useEffect, useState } from 'preact/hooks';
import { useRoute } from 'preact-iso';
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
        fetchHookInvocations(name, 50),
        fetchHookConfig(name).catch(() => null),
      ]);
      const entry = list.hooks.find((hook) => hook.name === name) || null;
      setState((prev) => ({
        ...prev,
        entry,
        invocations,
        config,
        error: entry ? null : `hook ${name} is not registered`,
        loading: false,
      }));
    } catch (err) {
      setState((prev) => ({
        ...prev,
        error: err instanceof Error ? err.message : 'failed to load hook',
        loading: false,
      }));
    }
  }

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => {
      void load();
    }, POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [name]);

  async function handleReload() {
    setState((prev) => ({ ...prev, reloadStatus: 'reloading hook registry…' }));
    try {
      const response = await reloadHooks();
      setState((prev) => ({
        ...prev,
        reloadStatus: response.reloaded
          ? `reloaded ${response.hook_count} hook(s)`
          : `nothing to reload — ${response.reason ?? 'no config detected'}`,
      }));
      await load();
    } catch (err) {
      setState((prev) => ({
        ...prev,
        reloadStatus: `reload failed: ${err instanceof Error ? err.message : 'unknown error'}`,
      }));
    }
  }

  return (
    <Layout>
      <section class="page-header">
        <div>
          <p class="page-eyebrow">hook detail</p>
          <h1>{name}</h1>
        </div>
        <div class="page-header-actions">
          <a href="/hooks" class="wb-button">All hooks</a>
          <ReloadButton onConfirm={handleReload} disabled={state.loading} />
        </div>
      </section>

      {state.reloadStatus ? <div class="notice-banner">{state.reloadStatus}</div> : null}
      {state.error ? <div class="error-banner">{state.error}</div> : null}

      {state.loading && !state.entry ? (
        <div class="loading-copy">Loading hook detail…</div>
      ) : state.entry ? (
        <div class="hook-detail-grid">
          <section class="surface-card hook-detail-summary">
            <h2>Registry status</h2>
            <dl class="meta-list">
              <dt>State</dt>
              <dd>
                <span class={state.entry.auto_disabled ? 'wb-badge wb-badge-failed' : state.entry.enabled ? 'wb-badge wb-badge-completed' : 'wb-badge wb-badge-default'}>
                  {state.entry.auto_disabled ? 'auto-disabled' : state.entry.enabled ? 'enabled' : 'disabled'}
                </span>
              </dd>
              <dt>Events</dt>
              <dd><span class="mono-copy">{state.entry.events.join(' · ') || 'manual dispatch only'}</span></dd>
              <dt>Action</dt>
              <dd><span class="mono-chip">{state.entry.action_type}</span></dd>
              <dt>Error rate</dt>
              <dd>{errorRatePercent(state.entry.successes, state.entry.failures)}%</dd>
              <dt>Last invoked</dt>
              <dd>{state.entry.last_invoked_at ? formatTimestamp(state.entry.last_invoked_at, true) : 'never'}</dd>
            </dl>
          </section>

          <section class="surface-card hook-config-card">
            <div class="section-heading compact-heading">
              <div>
                <p class="section-kicker">configuration</p>
                <h2>YAML</h2>
              </div>
            </div>
            {state.config?.yaml ? (
              <pre class="code-panel">{state.config.yaml}</pre>
            ) : state.config?.note ? (
              <p class="muted">{state.config.note}</p>
            ) : (
              <EmptyState
                title="no config materialized"
                detail="reload the registry after updating the coordinator hook config path."
                inline
              />
            )}
          </section>
        </div>
      ) : null}

      <section class="surface-card hook-detail-timeline">
        <div class="section-heading compact-heading">
          <div>
            <p class="section-kicker">invocation history</p>
            <h2>Chronological timeline</h2>
          </div>
        </div>
        {state.invocations ? (
          <HookInvocationTimeline invocations={state.invocations.invocations} />
        ) : (
          <div class="loading-copy">Loading invocation history…</div>
        )}
      </section>
    </Layout>
  );
}

export default HookDetail;
