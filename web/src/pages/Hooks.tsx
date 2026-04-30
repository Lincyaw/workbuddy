import { useEffect, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { fetchHookInvocations, fetchHooks, reloadHooks, type HooksListResponse } from '../api/hooks';
import { EmptyState } from '../components/EmptyState';
import { HookList } from '../components/HookList';
import { Layout } from '../components/Layout';
import { ReloadButton } from '../components/ReloadButton';

const POLL_INTERVAL_MS = 30_000;

interface FetchState {
  data: HooksListResponse | null;
  latencySeries: Record<string, number[]>;
  error: string | null;
  loading: boolean;
}

const INITIAL: FetchState = { data: null, latencySeries: {}, error: null, loading: true };

export function Hooks() {
  const { route } = useLocation();
  const [state, setState] = useState<FetchState>(INITIAL);
  const [reloadStatus, setReloadStatus] = useState<string | null>(null);

  async function load() {
    try {
      const data = await fetchHooks();
      const entries = await Promise.all(
        data.hooks.map(async (hook) => {
          try {
            const invocations = await fetchHookInvocations(hook.name, 50);
            return [hook.name, invocations.invocations.map((item) => item.duration_ms)] as const;
          } catch {
            return [hook.name, []] as const;
          }
        }),
      );
      setState({ data, latencySeries: Object.fromEntries(entries), error: null, loading: false });
    } catch (error) {
      const message = error instanceof Error ? error.message : 'failed to load hooks';
      setState((current) => ({ ...current, error: message, loading: false }));
    }
  }

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, []);

  async function handleReload() {
    setReloadStatus('reloading hook configuration...');
    try {
      const response = await reloadHooks();
      setReloadStatus(response.reloaded ? `reloaded ${response.hook_count} hook(s)` : `nothing to reload — ${response.reason || 'no config found'}`);
      await load();
    } catch (error) {
      setReloadStatus(`reload failed: ${error instanceof Error ? error.message : 'unknown error'}`);
    }
  }

  const data = state.data;

  return (
    <Layout>
      <section class="wb-stack">
        <header class="flex flex-wrap items-end justify-between gap-3">
          <div>
            <p class="wb-section-label">hooks registry</p>
            <h1 class="wb-page-title">hooks</h1>
            <p class="wb-page-copy">Registered dispatch hooks, their most recent outcomes, and a latency sparkline for the last fifty invocations.</p>
          </div>
          <ReloadButton onConfirm={handleReload} disabled={state.loading} />
        </header>

        {reloadStatus ? <div class="wb-panel">{reloadStatus}</div> : null}
        {state.error ? <div class="wb-panel text-state-danger">{state.error}</div> : null}

        {state.loading && !data ? (
          <EmptyState glyph="loading" title="loading hooks" copy="Reading hook configuration, counters, and recent latency samples." />
        ) : !data || data.hooks.length === 0 ? (
          <EmptyState
            glyph="hooks"
            title="no hooks registered"
            copy="point `~/.config/workbuddy/hooks.yaml` at this coordinator and reload."
          />
        ) : (
          <>
            <section class="wb-panel flex flex-wrap items-center gap-3 text-[13px] text-text-secondary">
              {data.config_path ? <span>config <span class="wb-id-pill">{data.config_path}</span></span> : null}
              <span>overflow <strong class="text-text-primary">{data.overflow_total}</strong></span>
              <span>dropped <strong class="text-text-primary">{data.dropped_total}</strong></span>
            </section>
            <section class="wb-table-shell p-4">
              <HookList hooks={data.hooks} latencySeries={state.latencySeries} onSelect={(name) => route(`/hooks/${encodeURIComponent(name)}`)} />
            </section>
          </>
        )}
      </section>
    </Layout>
  );
}

export default Hooks;
