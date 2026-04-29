// Default dev↔review cap from internal/statemachine.DefaultMaxReviewCycles.
// Per-workflow overrides exist on the Go side but aren't exposed on the
// in-flight payload; the dashboard treats this as the orchestrator-level
// fallback (and tells the operator that). When the API grows a per-issue
// `max_review_cycles` field, swap this constant out for a row-level value.
export const DEFAULT_MAX_REVIEW_CYCLES = 3;

// devReviewCycleCount derives the dev↔review round-trip count from the
// cycle_counts map returned by /api/v1/issues/in-flight. We use the
// developing→reviewing edge: every dev pass that ships a PR for review
// increments it, so it lines up with the operator's "how many handoffs has
// dev produced" mental model.
export function devReviewCycleCount(counts: Record<string, number> | undefined): number {
  if (!counts) return 0;
  return counts['developing->reviewing'] ?? 0;
}
