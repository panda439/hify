import { create } from "zustand";
import { setAccessToken } from "@/lib/api";

// Field names match the backend's JSON snake_case convention verbatim
// (see internal/user/dto.go) — no camelCase remapping on the wire.
export interface CurrentUser {
  id: string;
  email: string;
  display_name: string;
  role: "admin" | "member";
}

export type AuthStatus = "idle" | "authenticated" | "unauthenticated";

interface AuthState {
  user: CurrentUser | null;
  status: AuthStatus;
  login: (accessToken: string, user: CurrentUser) => void;
  logout: () => void;
  setUnauthenticated: () => void;
}

export const useAuthStore = create<AuthState>((set) => ({
  user: null,
  status: "idle",
  login: (accessToken, user) => {
    setAccessToken(accessToken);
    set({ user, status: "authenticated" });
  },
  logout: () => {
    setAccessToken(null);
    set({ user: null, status: "unauthenticated" });
  },
  setUnauthenticated: () => {
    setAccessToken(null);
    set({ user: null, status: "unauthenticated" });
  },
}));
