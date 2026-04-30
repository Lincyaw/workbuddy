import { useEffect, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { Layout } from '../components/Layout';
import { HookList } from '../components/HookList';
import { ReloadButton } from '../components/ReloadButton';
import { EmptyState } from '../components/EmptyState';
import { fetchHooks, reloadHooks, type HooksListResponse } from '../api/hooks';

const POLL_INTERVAL_MS = 30_000;

interface FetchState {
  data: HooksListResponse | null;
  error: string | null;
  loading: boolean;
}

const INITIAL: FetchState = { data: null, error: null, loading: true };

export function Hooks() {
  const { route } = useLocation();
  const [state, setState] = useState<FetchState>(INITIAL);
  const [reloadStatus, setReloadStatus] = useState<string | null>(null);

  async function load() {
    try {
      const data = await fetchHooks();
      setState({ data, error: null, loading: false });
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'failed to load hooks';
      setState((prev) => ({ ...prev, error: msg, loading: false }));
    }
  }

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      await load();
      if (cancelled) return;
    })();
    const timer = setInterval(() => {
      void load();
    }, POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, []);

  async function handleReload() {
    setReloadStatus('Reloading hook configuration...');
    try {
      const resp = await reloadHooks();
      if (!resp.reloaded) {
        setReloadStatus(`Nothing to reload - ${resp.reason ?? 'no config found'}`);
      } else {
        const warnings = resp.warnings?.length
          ? ` with ${resp.warnings.length} warning${resp.warnings.length === 1 ? '' : 's'}`
          : '';
        setReloadStatus(`Reloaded ${resp.hook_count} hook(s)${warnings}.`);
      }
      await load();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'reload failed';
      setReloadStatus(`Reload failed: ${msg}`);
    }
  }

  const data = state.data;
  return (
    <Layout>
      <div class="wb-page-header wb-page-header--split">
        <div>
          <p class="wb-eyebrow">Automation</p>
          <h1 class="wb-page-title">Hooks</h1>
          <p class="wb-page-subtitle">Inspect dispatcher bindings, failure rates, and the hooks currently loaded from config.</p>
        </div>
        <ReloadButton onConfirm={handleReload} disabled={state.loading} />
      </div>

      {reloadStatus && <div class="wb-alert wb-alert--info">{reloadStatus}</div>}
      {state.error && <div class="wb-alert wb-alert--danger">{state.error}</div>}

      <div class="wb-card wb-card--sm wb-stack-sm">
        {data?.config_path ? <div>Config path <code class="wb-code-pill">{data.config_path}</code></div> : null}
        {data ? (
          <div class="wb-inline-metrics wb-num">
            <span>Dispatcher overflow <strong>{data.overflow_total}</strong></span>
            <span>Per-hook drops <strong>{data.dropped_total}</strong></span>
          </div>
        ) : null}
      </div>

      <div class="wb-table-card">
        {state.loading && !data ? (
          <EmptyState icon=".." title="Loading hooks" copy="Reading hook configuration, invocation counters, and dispatcher state." />
        ) : !data || data.hooks.length === 0 ? (
          <EmptyState
            icon="[]"
            title="No hooks are configured yet"
            copy={<>Add entries to <code class="wb-code-pill">docs/hooks.md</code> and reload the dispatcher to see them here.</>}
          />
        ) : (
          <HookList hooks={data.hooks} onSelect={(name) => route(`/hooks/${encodeURIComponent(name)}`)} />
        )}
      </div>
    </Layout>
  );
}

export default Hooks;
