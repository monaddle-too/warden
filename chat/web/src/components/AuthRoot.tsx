import { useCallback, useEffect, useState } from "react";
import { Shield } from "lucide-react";
import {
  GoogleLogin,
  type AuthState,
  type BrowserSession,
} from "./GoogleLogin";
import { ChatShell } from "./ChatShell";
import { remoteSession } from "../api";
export function AuthRoot() {
  const [auth, setAuth] = useState<AuthState>();
  const [error, setError] = useState("");
  const accept = useCallback((session: BrowserSession) => {
    remoteSession(session.csrf);
    setAuth({ enabled: true, ...session });
    const next = new URLSearchParams(location.search).get("next");
    if (next) {
      const url = new URL(next, location.origin);
      if (url.origin === location.origin && url.pathname === "/auth/preview") {
        location.replace(url.href);
      }
    }
  }, []);
  const load = useCallback(async () => {
    try {
      const response = await fetch("/auth/session");
      if (!response.ok) throw new Error("Warden sign-in is unavailable");
      const state: AuthState = await response.json();
      if (state.user && state.csrf) accept(state as BrowserSession);
      else setAuth(state);
      setError("");
    } catch (e) {
      setError(String(e));
    }
  }, [accept]);
  useEffect(() => {
    void load();
    const refresh = () => {
      remoteSession("");
      void load();
    };
    window.addEventListener("warden-session-expired", refresh);
    return () => window.removeEventListener("warden-session-expired", refresh);
  }, [load]);
  async function logout() {
    if (!auth?.csrf) return;
    await fetch("/auth/logout", {
      method: "POST",
      headers: { "X-Warden-CSRF": auth.csrf },
    });
    remoteSession("");
    await load();
  }
  if (!auth || (auth.enabled && !auth.user))
    return (
      <div className="signin">
        <Shield size={36} />
        <h1>Warden</h1>
        <p>Sign in to access your agents and private previews.</p>
        {auth && <GoogleLogin auth={auth} onSession={accept} onRetry={load} />}
        <p role="alert">{error}</p>
      </div>
    );
  return (
    <ChatShell
      canConnectGoogle={!auth.enabled || auth.user?.role === "admin"}
      admin={!auth.enabled || auth.user?.role === "admin"}
      signIn={auth.enabled}
      account={
        auth.enabled ? (
          <div className="warden-account">
            <span className="warden-account-avatar" aria-hidden="true">
              {(auth.user?.email || "?").slice(0, 1)}
            </span>
            <small title={auth.user?.email}>{auth.user?.email}</small>
            <button className="ghost" onClick={logout}>
              Sign out
            </button>
          </div>
        ) : undefined
      }
    />
  );
}
