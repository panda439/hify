import { refreshAccessToken } from "@/lib/api";
import { useAuthStore, type CurrentUser } from "@/stores/auth";

const DEV_PREVIEW_USER: CurrentUser = {
  id: "dev-preview",
  email: "dev@hify.local",
  display_name: "预览用户",
  role: "admin",
};

// Called once on app boot: the refresh token lives in an httpOnly cookie, so
// a page reload has no in-memory access token until we exchange the cookie
// for a fresh one. /auth/refresh returns the user profile directly, so no
// separate /users/me round trip is needed here.
export async function restoreSession(): Promise<void> {
  const user = await refreshAccessToken();
  if (user) {
    useAuthStore.setState({ user, status: "authenticated" });
    return;
  }

  if (import.meta.env.DEV && (await isBackendUnreachable())) {
    // Dev-only convenience: falls back to a mock user so the UI can be
    // built/previewed without a running backend. This only activates when
    // the backend is genuinely unreachable (health check fails) — with the
    // backend up, a missing/expired refresh cookie correctly falls through
    // to setUnauthenticated() below and the user sees the real login page.
    useAuthStore.setState({ user: DEV_PREVIEW_USER, status: "authenticated" });
    return;
  }

  useAuthStore.getState().setUnauthenticated();
}

async function isBackendUnreachable(): Promise<boolean> {
  try {
    const res = await fetch("/api/v1/health");
    return !res.ok;
  } catch {
    return true;
  }
}
