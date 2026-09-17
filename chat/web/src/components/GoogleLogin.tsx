"use client";
import { useEffect, useRef, useState } from "react";

export type BrowserSession = {
  user: { sub: string; email: string; role: string; name?: string };
  csrf: string;
};
export type AuthState = Partial<BrowserSession> & {
  enabled: boolean;
  client_id?: string;
  nonce?: string;
};
type GoogleIdentity = {
  initialize: (options: {
    client_id: string;
    nonce: string;
    callback: (response: { credential: string }) => void;
    auto_select: boolean;
  }) => void;
  renderButton: (
    element: HTMLElement,
    options: Record<string, string | number>,
  ) => void;
  disableAutoSelect: () => void;
};
declare global {
  interface Window {
    google?: { accounts: { id: GoogleIdentity } };
  }
}
let googleScript: Promise<void> | undefined;
function loadGoogle() {
  if (window.google?.accounts.id) return Promise.resolve();
  if (!googleScript) {
    googleScript = new Promise<void>((resolve, reject) => {
      const script = document.createElement("script");
      script.src = "https://accounts.google.com/gsi/client";
      script.async = true;
      script.onload = () => resolve();
      script.onerror = () => {
        googleScript = undefined;
        script.remove();
        reject(
          new Error(
            "Google sign-in could not load. Check your connection and reload.",
          ),
        );
      };
      document.head.appendChild(script);
    });
  }
  return googleScript;
}

export function GoogleLogin({
  auth,
  onSession,
  onRetry,
}: {
  auth: AuthState;
  onSession: (session: BrowserSession) => void;
  onRetry: () => void;
}) {
  const button = useRef<HTMLDivElement>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (!auth.client_id || !auth.nonce) return;
    let cancelled = false;
    void loadGoogle()
      .then(() => {
        if (cancelled || !button.current) return;
        window.google!.accounts.id.initialize({
          client_id: auth.client_id!,
          nonce: auth.nonce!,
          auto_select: false,
          callback: async ({ credential }) => {
            if (cancelled) return;
            setBusy(true);
            setError("");
            try {
              const response = await fetch("/auth/google", {
                method: "POST",
                credentials: "same-origin",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ credential }),
                signal: AbortSignal.timeout(15000),
              });
              const data = (await response.json()) as BrowserSession & {
                error?: string;
              };
              if (!response.ok)
                throw new Error(data.error || "Sign-in failed.");
              if (!cancelled) onSession(data as BrowserSession);
            } catch (e) {
              if (!cancelled)
                setError(e instanceof Error ? e.message : "Sign-in failed.");
            } finally {
              if (!cancelled) setBusy(false);
            }
          },
        });
        button.current.replaceChildren();
        window.google!.accounts.id.renderButton(button.current, {
          theme: "outline",
          size: "large",
          text: "signin_with",
          shape: "rectangular",
          width: 280,
        });
      })
      .catch((e) => {
        if (!cancelled) setError(String(e));
      });
    return () => {
      cancelled = true;
    };
  }, [auth.client_id, auth.nonce, onSession]);
  return (
    <>
      <div className="google-button" ref={button} hidden={busy || !!error} />
      {busy && <output>Signing in…</output>}
      {error && (
        <div role="alert">
          <p className="server-error">{error}</p>
          <button onClick={onRetry}>Try again</button>
        </div>
      )}
    </>
  );
}
