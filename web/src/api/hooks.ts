// Hooks API client. Mirrors the JSON shapes in internal/app/hooks_api.go.
// Update both sides together when extending the contract.

import { apiFetch, apiJSON } from './client';

export interface HookListEntry {
  name: string;
  events: string[];
  action_type: string;
  enabled: boolean;
  auto_disabled: boolean;
  successes: number;
  failures: number;
  filtered: number;
  disabled_drops: number;
  overflow: number;
  consecutive_failures: number;
  last_error?: string;
  last_failure_at?: string;
  last_invoked_at?: string;
  duration_count: number;
  duration_sum_ns: number;
}

export interface HooksListResponse {
  config_path?: string;
  overflow_total: number;
  dropped_total: number;
  hooks: HookListEntry[];
}

export interface HookInvocation {
  started_at: string;
  finished_at: string;
  duration_ms: number;
  result: 'success' | 'failure' | 'overflow' | string;
  error?: string;
  event_type?: string;
  repo?: string;
  issue_num?: number;
  stdout?: string;
  stderr?: string;
}

export interface HookInvocationsResponse {
  hook: string;
  limit: number;
  invocations: HookInvocation[];
}

export interface HookConfigResponse {
  hook: string;
  config_path?: string;
  yaml?: string;
  note?: string;
}

export interface HookReloadResponse {
  reloaded: boolean;
  // Present when reloaded is false (e.g. "no config" on fresh installs).
  reason?: string;
  config_path: string;
  hook_count: number;
  warnings?: string[];
}

export function fetchHooks(): Promise<HooksListResponse> {
  return apiJSON<HooksListResponse>('/api/v1/hooks');
}

export function fetchHookInvocations(
  name: string,
  limit = 20,
): Promise<HookInvocationsResponse> {
  const sp = new URLSearchParams({ limit: String(limit) });
  return apiJSON<HookInvocationsResponse>(
    `/api/v1/hooks/${encodeURIComponent(name)}/invocations?${sp.toString()}`,
  );
}

export function fetchHookConfig(name: string): Promise<HookConfigResponse> {
  return apiJSON<HookConfigResponse>(
    `/api/v1/hooks/${encodeURIComponent(name)}/config`,
  );
}

export async function reloadHooks(): Promise<HookReloadResponse> {
  // Use apiFetch instead of apiJSON so we can detect non-JSON 503/etc bodies.
  const resp = await apiFetch('/api/v1/hooks/reload', { method: 'POST' });
  const text = await resp.text();
  let body: unknown = null;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = text;
    }
  }
  if (!resp.ok) {
    const msg =
      body && typeof body === 'object' && 'error' in (body as Record<string, unknown>)
        ? String((body as Record<string, unknown>).error)
        : `reload failed: ${resp.status}`;
    throw new Error(msg);
  }
  return body as HookReloadResponse;
}

// rate24h projects total counters into a normalized shape for the list view.
// We can't always show "last 24h" precisely (counters reset on reload, but
// the dispatcher exposes invocations only as a 100-entry ring), so the list
// view computes the rate from the per-hook invocations when available and
// falls back to lifetime counters otherwise.
export function errorRatePercent(
  successes: number,
  failures: number,
): number {
  const total = successes + failures;
  if (total === 0) return 0;
  return Math.round((failures / total) * 100);
}
