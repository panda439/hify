import type { ReactNode } from "react";
import { Navigate } from "react-router-dom";
import { useAuthStore } from "@/stores/auth";

// status === "idle" means restoreSession() (see lib/session.ts) hasn't
// resolved yet — render nothing rather than bouncing to /login and back.
export function ProtectedRoute({ children }: { children: ReactNode }) {
  const status = useAuthStore((s) => s.status);

  if (status === "idle") return null;
  if (status === "unauthenticated") return <Navigate to="/login" replace />;
  return <>{children}</>;
}
