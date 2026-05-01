import type { RolloutGroup } from '../api/types';
import { formatDuration, formatTimestamp, shortID, statusBadgeClass } from '../lib/format';

interface RolloutGroupPanelProps {
  repo: string;
  issueNum: number;
  group: RolloutGroup | null;
  compareHref?: string;
  title?: string;
}

export function RolloutGroupPanel({
  repo,
  issueNum,
  group,
  compareHref,
  title = 'Rollout group',
}: RolloutGroupPanelProps) {
  if (!group || group.rollouts_total <= 1 || group.members.length <= 1) {
    return null;
  }
  const successCount = group.members.filter((member) => isSuccess(member.status)).length;
  const synthSummary = group.synth_outcome
    ? `synth ${group.synth_outcome.decision}`
    : 'synth pending';
  const aggregate = `${successCount}/${group.rollouts_total} succeeded · ${synthSummary}`;

  return (
    <section class="surface-card rollout-panel">
      <div class="section-heading compact-heading rollout-panel-header">
        <div>
          <p class="section-kicker">parallel rollouts</p>
          <h2>{title}</h2>
          <p class="rollout-panel-summary">
            {repo}#{issueNum} · rollouts:{group.rollouts_total} · {aggregate}
          </p>
        </div>
        {compareHref ? <a href={compareHref} class="wb-button wb-button-small">Compare</a> : null}
      </div>

      <div class="rollout-panel-grid" role="table" aria-label="Rollout group">
        <div class="rollout-panel-row rollout-panel-head" role="row">
          <span>Lane</span>
          <span>Status</span>
          <span>Pull request</span>
          <span>Session</span>
          <span>Worker</span>
          <span>Duration</span>
        </div>
        {group.members.map((member) => (
          <div class="rollout-panel-row" role="row" key={member.rollout_index}>
            <span data-label="Lane" class="rollout-lane-label">
              rollout {member.rollout_index}/{group.rollouts_total}
            </span>
            <span data-label="Status">
              <span class={statusBadgeClass(member.status)}>{member.status}</span>
            </span>
            <span data-label="Pull request">
              {member.pr_number > 0 ? (
                <a
                  href={`https://github.com/${repo}/pull/${member.pr_number}`}
                  target="_blank"
                  rel="noreferrer"
                  class="mono-link"
                >
                  #{member.pr_number}
                </a>
              ) : '—'}
            </span>
            <span data-label="Session">
              {member.session_id ? (
                <a href={`/sessions/${encodeURIComponent(member.session_id)}`} class="mono-link">
                  {shortID(member.session_id)}
                </a>
              ) : '—'}
            </span>
            <span data-label="Worker">{member.worker_id || '—'}</span>
            <span data-label="Duration">
              {formatDuration(member.duration_seconds)} · {member.ended_at
                ? formatTimestamp(member.ended_at, true)
                : formatTimestamp(member.started_at, true)}
            </span>
          </div>
        ))}
        {group.synth_outcome ? (
          <div class="rollout-panel-row rollout-panel-synth" role="row">
            <span data-label="Lane" class="rollout-lane-label">synth</span>
            <span data-label="Status">
              <span class={statusBadgeClass(synthBadgeStatus(group.synth_outcome.decision))}>
                {group.synth_outcome.decision}
              </span>
            </span>
            <span data-label="Pull request">
              {group.synth_outcome.chosen_pr ? (
                <a
                  href={`https://github.com/${repo}/pull/${group.synth_outcome.chosen_pr}`}
                  target="_blank"
                  rel="noreferrer"
                  class="mono-link"
                >
                  chose #{group.synth_outcome.chosen_pr}
                </a>
              ) : group.synth_outcome.synth_pr ? (
                <a
                  href={`https://github.com/${repo}/pull/${group.synth_outcome.synth_pr}`}
                  target="_blank"
                  rel="noreferrer"
                  class="mono-link"
                >
                  synth #{group.synth_outcome.synth_pr}
                </a>
              ) : '—'}
            </span>
            <span data-label="Session">
              {group.synth_outcome.reason || 'decision recorded'}
            </span>
            <span data-label="Worker">—</span>
            <span data-label="Duration">{formatTimestamp(group.synth_outcome.ts, true)}</span>
          </div>
        ) : null}
      </div>
    </section>
  );
}

function isSuccess(status: string): boolean {
  return (status || '').toLowerCase() === 'completed';
}

function synthBadgeStatus(decision: string): string {
  return decision === 'escalate' ? 'failed' : 'completed';
}
