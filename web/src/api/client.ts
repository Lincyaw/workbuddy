import type {
  InFlightIssue,
  IssueDetail,
  StatusResponse,
} from './types';

export interface ApiError extends Error {
  status: number;
  body: unknown;
}

function buildLoginRedirect(): string {
  const next = encodeURIComponent(
    window.location.pathname + window.location.search + window.location.hash,
  );
  return `/login?next=${next}`;
}

export async function apiFetch(input: string, init: RequestInit = {}): Promise<Response> {
  const resp = await fetch(input, {
    ...init,
    credentials: 'include',
    headers: {
      Accept: 'application/json',
      ...(init.headers ?? {}),
    },
  });

  if (resp.status === 401) {
    window.location.replace(buildLoginRedirect());
    return resp;
  }
  return resp;
}

export async function apiJSON<T>(input: string, init: RequestInit = {}): Promise<T> {
  const resp = await apiFetch(input, init);
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
    const err = new Error(`request failed: ${resp.status}`) as ApiError;
    err.status = resp.status;
    err.body = body;
    throw err;
  }
  return body as T;
}

export function getStatus(): Promise<StatusResponse> {
  return apiJSON<StatusResponse>('/api/v1/status');
}

export function getInFlightIssues(): Promise<InFlightIssue[]> {
  return apiJSON<InFlightIssue[]>('/api/v1/issues/in-flight');
}

export function getIssueDetail(
  owner: string,
  repo: string,
  num: number | string,
): Promise<IssueDetail> {
  const path = `/api/v1/issues/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/${encodeURIComponent(String(num))}`;
  return apiJSON<IssueDetail>(path);
}

// logout posts to /logout. The server clears the cookie and returns a 302
// redirect to /login; we follow up by navigating the browser to /login so the
// SPA shell unmounts cleanly even when the response is opaque (3xx after
// follow, or fetch quirks).
export async function logout(): Promise<void> {
  try {
    await fetch('/logout', {
      method: 'POST',
      credentials: 'include',
      redirect: 'manual',
    });
  } catch {
    // network errors are not fatal — we still want to land on /login.
  }
  window.location.assign('/login');
}
