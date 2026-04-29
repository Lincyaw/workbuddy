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
