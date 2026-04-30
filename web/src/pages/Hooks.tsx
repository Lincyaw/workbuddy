import { useEffect, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { Layout } from '../components/Layout';
import { HookList } from '../components/HookList';
import { ReloadButton } from '../components/ReloadButton';
import {
  fetchHooks,
  reloadHooks,
  type HooksListResponse,
} from '../api/hooks';

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
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function handleReload() {
    setReloadStatus('reloading…');
    try {
      const resp = await reloadHooks();
      const warnings = resp.warnings?.length
        ? ` (with ${resp.warnings.length} warning${resp.warnings.length === 1 ? '' : 's'})`
        : '';
      setReloadStatus(`reloaded ${resp.hook_count} hook(s)${warnings}`);
      await load();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'reload failed';
      setReloadStatus(`reload failed: ${msg}`);
    }
  }

  const data = state.data;
  return (
    <Layout>
      <div class="wb-hooks-header">
        <h1>Hooks</h1>
        <ReloadButton onConfirm={handleReload} disabled={state.loading} />
      </div>
      {reloadStatus && <div class="wb-hooks-status">{reloadStatus}</div>}
      {state.error && <div class="error-banner">{state.error}</div>}
      <p class="muted" style={{ marginTop: 0 }}>
        {data?.config_path ? <>Config: <code class="code-chip">{data.config_path}</code></> : null}
        {data ? <span style={{ marginLeft: 12 }}>
          dispatcher overflow: <strong>{data.overflow_total}</strong>{' '}· per-hook drops: <strong>{data.dropped_total}</strong>
        </span> : null}
      </p>
      <div class="panel">
        {state.loading && !data ? (
          <div class="empty">Loading…</div>
        ) : !data || data.hooks.length === 0 ? (
          <div class="empty">No hooks registered. Add entries to your hooks YAML and click Reload.</div>
        ) : (
          <HookList
            hooks={data.hooks}
            onSelect={(name) => route(`/hooks/${encodeURIComponent(name)}`)}
          />
        )}
      </div>
    </Layout>
  );
}

export default Hooks;
