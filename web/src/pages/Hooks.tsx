import { useEffect, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { Layout } from '../components/Layout';
import { HookList } from '../components/HookList';
import { ReloadButton } from '../components/ReloadButton';
import { EmptyState } from '../components/EmptyState';
import {
  fetchHookInvocations,
  fetchHooks,
  reloadHooks,
  type HooksListResponse,
} from '../api/hooks';

const POLL_INTERVAL_MS = 30_000;

interface FetchState {
  data: HooksListResponse | null;
  latencySamples: Record<string, number[]>;
  error: string | null;
  loading: boolean;
}

const INITIAL: FetchState = { data: null, latencySamples: {}, error: null, loading: true };

export function Hooks() {
  const { route } = useLocation();
  const [state, setState] = useState<FetchState>(INITIAL);
  const [reloadStatus, setReloadStatus] = useState<string | null>(null);

  async function load() {
    try {
      const data = await fetchHooks();
      const samples = Object.fromEntries(
        await Promise.all(
          (data.hooks || []).map(async (hook) => {
            try {
              const invocations = await fetchHookInvocations(hook.name, 50);
              return [hook.name, invocations.invocations.map((item) => item.duration_ms)] as const;
            } catch {
              return [hook.name, []] as const;
            }
          }),
        ),
      );
      setState({ data, latencySamples: samples, error: null, loading: false });
    } catch (err) {
      setState((prev) => ({
        ...prev,
        error: err instanceof Error ? err.message : 'failed to load hooks',
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
  }, []);

  async function handleReload() {
    setReloadStatus('reloading hook registry…');
    try {
      const response = await reloadHooks();
      setReloadStatus(
        response.reloaded
          ? `reloaded ${response.hook_count} hook(s)`
          : `nothing to reload — ${response.reason ?? 'no config detected'}`,
      );
      await load();
    } catch (err) {
      setReloadStatus(`reload failed: ${err instanceof Error ? err.message : 'unknown error'}`);
    }
  }

  return (
    <Layout>
      <section class="page-header">
        <div>
          <p class="page-eyebrow">automation hooks</p>
          <h1>Hooks</h1>
        </div>
        <ReloadButton onConfirm={handleReload} disabled={state.loading} />
      </section>

      {reloadStatus ? <div class="notice-banner">{reloadStatus}</div> : null}
      {state.error ? <div class="error-banner">{state.error}</div> : null}

      <section class="surface-card hooks-summary-card">
        <div class="hook-summary-line">
          <span>config</span>
          <strong class="mono-chip">{state.data?.config_path || '~/.config/workbuddy/hooks.yaml'}</strong>
        </div>
        <div class="hook-summary-line">
          <span>dispatcher overflow</span>
          <strong>{state.data?.overflow_total ?? 0}</strong>
        </div>
        <div class="hook-summary-line">
          <span>per-hook drops</span>
          <strong>{state.data?.dropped_total ?? 0}</strong>
        </div>
      </section>

      {state.loading && !state.data ? (
        <div class="loading-copy">Loading hook telemetry…</div>
      ) : !state.data || state.data.hooks.length === 0 ? (
        <section class="surface-card">
          <EmptyState
            title="no hooks registered"
            detail="point ~/.config/workbuddy/hooks.yaml at this coordinator and reload."
            inline
          />
        </section>
      ) : (
        <HookList
          hooks={state.data.hooks}
          latencySamples={state.latencySamples}
          onSelect={(name) => route(`/hooks/${encodeURIComponent(name)}`)}
        />
      )}
    </Layout>
  );
}

export default Hooks;
