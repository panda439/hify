// Typed fetch client. Access token lives in memory only (never
// localStorage); the refresh token is an httpOnly cookie the browser sends
// automatically via `credentials: "include"`. On a 401 we transparently
// retry once after refreshing, deduping concurrent refreshes into one
// in-flight request so a burst of parallel calls doesn't hit
// /auth/refresh multiple times.

import type { CurrentUser } from "@/stores/auth";

const API_BASE = "/api/v1";

export class ApiError extends Error {
  status: number;
  code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

let accessToken: string | null = null;

export function setAccessToken(token: string | null): void {
  accessToken = token;
}

interface ErrorBody {
  error?: { code?: string; message?: string };
}

async function request<T>(path: string, init: RequestInit = {}, allowRetry = true): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body !== undefined) headers.set("Content-Type", "application/json");
  if (accessToken) headers.set("Authorization", `Bearer ${accessToken}`);

  const res = await fetch(`${API_BASE}${path}`, {
    ...init,
    headers,
    credentials: "include",
  });

  if (res.status === 401 && allowRetry) {
    const user = await refreshAccessToken();
    if (user) {
      return request<T>(path, init, false);
    }
  }

  if (!res.ok) {
    const body: ErrorBody | null = await res.json().catch(() => null);
    throw new ApiError(
      res.status,
      body?.error?.code ?? "unknown_error",
      body?.error?.message ?? res.statusText,
    );
  }

  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

interface RefreshResponse {
  access_token: string;
  user: CurrentUser;
}

let refreshPromise: Promise<CurrentUser | null> | null = null;

// Returns the refreshed user (also used by lib/session.ts to restore a
// session on page load without a second /users/me round trip) or null if
// the refresh cookie is missing/expired/invalid.
async function refreshAccessToken(): Promise<CurrentUser | null> {
  refreshPromise ??= fetch(`${API_BASE}/auth/refresh`, {
    method: "POST",
    credentials: "include",
  })
    .then(async (res) => {
      if (!res.ok) {
        setAccessToken(null);
        return null;
      }
      const data = (await res.json()) as RefreshResponse;
      setAccessToken(data.access_token);
      return data.user;
    })
    .catch(() => null)
    .finally(() => {
      refreshPromise = null;
    });

  return refreshPromise;
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: "POST", body: body !== undefined ? JSON.stringify(body) : undefined }),
  put: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: "PUT", body: body !== undefined ? JSON.stringify(body) : undefined }),
  delete: <T>(path: string) => request<T>(path, { method: "DELETE" }),
};

export { refreshAccessToken };
