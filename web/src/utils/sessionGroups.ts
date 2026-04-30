// Group + label session list rows for the /sessions page.
//
// Sessions land on the dashboard as a flat list, but the operator's mental
// model is: "issue → which dev/review pass → which session". This module
// derives that structure on the client. Backend contract for
// /api/v1/sessions stays unchanged (per issue #251 non-goal).

import type { SessionListItem } from '../api/sessions';

export type SessionRole = 'dev' | 'review' | 'other';

export interface DecoratedSession extends SessionListItem {
  role: SessionRole;
  cycle: number;
  rolloutLabel?: string;
}

export interface SessionGroup {
  issueNum: number;
  repo: string;
  sessions: DecoratedSession[];
  // Latest createdAt across the group's sessions; used to sort groups.
  latestCreatedAt: string;
}

// inferRole maps an agent name to dev/review. Agent names are user-defined,
// but workbuddy ships exactly two roles (`role:dev`, `role:review`) and the
// stock catalog uses agent names like `dev-agent` / `review-agent`. We
// pattern-match on the substring rather than require an exact name so
// project-specific agent names (e.g. `claude-dev`, `codex-reviewer`) still
// classify correctly.
export function inferRole(agentName: string): SessionRole {
  const n = (agentName || '').toLowerCase();
  if (n.includes('review')) return 'review';
  if (n.includes('dev')) return 'dev';
  return 'other';
}

function compareCreatedAsc(a: SessionListItem, b: SessionListItem): number {
  const at = a.created_at || '';
  const bt = b.created_at || '';
  if (at === bt) return 0;
  return at < bt ? -1 : 1;
}

// groupSessionsByIssue groups sessions by issue_num and labels each session
// with a (role, cycle) pair derived from chronological order within the
// group. Cycle counting per role: the Nth session of a given role is cycle
// N (1-indexed). This matches the operator UI ordering
// `dev (cycle 1) → review (cycle 1) → dev (cycle 2) → review (cycle 2)`.
export function groupSessionsByIssue(sessions: SessionListItem[]): SessionGroup[] {
  const byIssue = new Map<number, SessionListItem[]>();
  for (const s of sessions) {
    const arr = byIssue.get(s.issue_num) || [];
    arr.push(s);
    byIssue.set(s.issue_num, arr);
  }

  const groups: SessionGroup[] = [];
  for (const [issueNum, items] of byIssue) {
    const sorted = [...items].sort(compareCreatedAsc);
    const roleCounts: Record<SessionRole, number> = { dev: 0, review: 0, other: 0 };
    const decorated: DecoratedSession[] = sorted.map((s) => {
      const role = inferRole(s.agent_name);
      const rolloutIndex = s.rollout_index || 0;
      const rolloutsTotal = s.rollouts_total || 0;
      if (rolloutIndex > 0 && rolloutsTotal > 1) {
        return {
          ...s,
          role,
          cycle: rolloutIndex,
          rolloutLabel: `rollout ${rolloutIndex}/${rolloutsTotal}`,
        };
      }
      roleCounts[role] += 1;
      return { ...s, role, cycle: roleCounts[role] };
    });

    // Display order: cycle ascending, dev before review within a cycle, then
    // "other" agents last. Stable on created_at within those keys.
    const roleOrder: Record<SessionRole, number> = { dev: 0, review: 1, other: 2 };
    decorated.sort((a, b) => {
      if (a.cycle !== b.cycle) return a.cycle - b.cycle;
      if (a.role !== b.role) return roleOrder[a.role] - roleOrder[b.role];
      return compareCreatedAsc(a, b);
    });

    const latest = sorted.reduce(
      (acc, s) => (s.created_at && s.created_at > acc ? s.created_at : acc),
      sorted[0]?.created_at || '',
    );
    groups.push({
      issueNum,
      repo: sorted[0]?.repo || '',
      sessions: decorated,
      latestCreatedAt: latest,
    });
  }

  // Most-recently-active issue first.
  groups.sort((a, b) => {
    if (a.latestCreatedAt === b.latestCreatedAt) return b.issueNum - a.issueNum;
    return a.latestCreatedAt < b.latestCreatedAt ? 1 : -1;
  });
  return groups;
}

// distinctValues collects unique non-empty values for a field across a list,
// sorted alphabetically. Used to populate the repo/agent filter dropdowns.
export function distinctValues<T>(items: T[], pick: (t: T) => string | undefined): string[] {
  const set = new Set<string>();
  for (const it of items) {
    const v = pick(it);
    if (v) set.add(v);
  }
  return [...set].sort((a, b) => a.localeCompare(b));
}
