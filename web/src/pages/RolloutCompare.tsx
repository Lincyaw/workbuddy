import { useEffect, useMemo, useState } from 'preact/hooks';
import { useLocation, useRoute } from 'preact-iso';
import { getIssueRolloutDiff, getIssueRollouts } from '../api/client';
import type { RolloutGroup } from '../api/types';
import { Layout } from '../components/Layout';

interface DiffState {
  index: number;
  diff: string;
  error?: string;
}

function parseSelected(raw: Record<string, string>): number[] {
  return ['a', 'b', 'c']
    .map((key) => Number(raw[key] || '0'))
    .filter((value, pos, arr) => Number.isFinite(value) && value > 0 && arr.indexOf(value) === pos)
    .slice(0, 3);
}

export function RolloutCompare() {
  const { params } = useRoute();
  const location = useLocation();
  const owner = params.owner;
  const repo = params.repo;
  const num = Number(params.num || '0');
  const [group, setGroup] = useState<RolloutGroup | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [diffs, setDiffs] = useState<DiffState[]>([]);
  const selected = useMemo(() => parseSelected(location.query), [location.query]);

  useEffect(() => {
    if (!owner || !repo || !Number.isFinite(num) || num <= 0) {
      setError('invalid issue path');
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    getIssueRollouts(owner, repo, num)
      .then((response) => {
        if (cancelled) return;
        setGroup(response);
        setError(null);
      })
      .catch((err) => {
        if (!cancelled) setError(err instanceof Error ? err.message : 'failed to load rollout group');
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [num, owner, repo]);

  useEffect(() => {
    if (!owner || !repo || !Number.isFinite(num) || num <= 0 || selected.length === 0) {
      setDiffs([]);
      return;
    }
    let cancelled = false;
    Promise.all(
      selected.map(async (index) => {
        try {
          const diff = await getIssueRolloutDiff(owner, repo, num, index);
          return { index, diff } satisfies DiffState;
        } catch (err) {
          return {
            index,
            diff: '',
            error: err instanceof Error ? err.message : 'failed to load diff',
          } satisfies DiffState;
        }
      }),
    ).then((items) => {
      if (!cancelled) setDiffs(items);
    });
    return () => {
      cancelled = true;
    };
  }, [num, owner, repo, selected.join(',')]);

  const members = group?.members || [];

  function updateSelection(slot: 'a' | 'b' | 'c', value: string) {
    const next = new URLSearchParams();
    Object.entries(location.query).forEach(([key, current]) => {
      if (current) next.set(key, current);
    });
    if (!value) {
      next.delete(slot);
    } else {
      next.set(slot, value);
    }
    location.route(`/issues/${owner}/${repo}/${num}/rollouts/compare?${next.toString()}`);
  }

  return (
    <Layout>
      <section class="page-header page-header-tight">
        <div>
          <p class="page-eyebrow">rollout comparator</p>
          <h1>{owner && repo ? `${owner}/${repo}#${num}` : 'Rollout compare'}</h1>
        </div>
      </section>

      {error ? <div class="error-banner">{error}</div> : null}
      {loading && !group ? <div class="loading-copy">Loading rollout diffs…</div> : null}

      <section class="surface-card compare-toolbar">
        <div class="section-heading compact-heading">
          <div>
            <p class="section-kicker">side by side</p>
            <h2>Compare candidate PR diffs</h2>
          </div>
        </div>
        <div class="compare-selects">
          {(['a', 'b', 'c'] as const).map((slot, index) => (
            <label key={slot}>
              <span>Column {index + 1}</span>
              <select value={location.query[slot] || ''} onChange={(event) => updateSelection(slot, (event.target as HTMLSelectElement).value)}>
                <option value="">None</option>
                {members.map((member) => (
                  <option key={member.rollout_index} value={String(member.rollout_index)}>
                    rollout {member.rollout_index}/{group?.rollouts_total}
                  </option>
                ))}
              </select>
            </label>
          ))}
        </div>
      </section>

      <section class={`compare-grid compare-grid-${Math.max(diffs.length, 1)}`}>
        {diffs.map((item) => (
          <article key={item.index} class="surface-card compare-column">
            <div class="section-heading compact-heading">
              <div>
                <p class="section-kicker">rollout {item.index}</p>
                <h2>PR diff</h2>
              </div>
            </div>
            {item.error ? (
              <div class="error-banner">{item.error}</div>
            ) : (
              <pre class="code-panel compare-diff-panel">{item.diff || '(empty diff)'}</pre>
            )}
          </article>
        ))}
      </section>
    </Layout>
  );
}

export default RolloutCompare;
