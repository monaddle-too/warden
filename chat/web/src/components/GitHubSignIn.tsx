import { useEffect, useRef, useState, type ReactNode } from "react";
import { Check, Copy, ExternalLink, GitBranch } from "lucide-react";
import { api } from "../api";

/* The GitHub sign-in from the browser (user-token mode): the policy
   service runs GitHub's device flow, the same one `warden login github`
   runs. We show the code, link to GitHub's verification page and poll
   until the sign-in lands or fails. Owner-only at the edge: whoever types
   the code binds their GitHub account to this Warden. */
type SignIn = {
  status: "none" | "pending" | "done" | "failed";
  user_code?: string;
  verification_uri?: string;
  expires_at?: number;
  login?: string;
  error?: string;
};

export function GitHubSignIn({
  connected,
  disabled,
  onSignedIn,
  children,
}: {
  // A stored sign-in exists: the button refreshes it instead of starting one.
  connected: boolean;
  disabled?: boolean;
  onSignedIn?: (login: string) => void;
  // Rendered beside the sign-in button while no attempt is pending (the
  // console's Disconnect button).
  children?: ReactNode;
}) {
  const [signIn, setSignIn] = useState<SignIn>({ status: "none" });
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  const copyTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(copyTimer.current), []);
  // The clipboard is unavailable outside secure contexts; the code stays
  // on screen to copy by hand.
  const copy = (code: string) =>
    void navigator.clipboard?.writeText(code).then(
      () => {
        setCopied(true);
        clearTimeout(copyTimer.current);
        copyTimer.current = setTimeout(() => setCopied(false), 1500);
      },
      () => {},
    );
  const signedIn = useRef(onSignedIn);
  signedIn.current = onSignedIn;
  // A reloaded page picks up an attempt that is still pending.
  useEffect(() => {
    let active = true;
    api<SignIn>("sharing/github_login_status")
      .then((s) => {
        if (active && s.status === "pending") setSignIn(s);
      })
      .catch(() => {});
    return () => {
      active = false;
    };
  }, []);
  useEffect(() => {
    if (signIn.status !== "pending") return;
    let active = true;
    const timer = setInterval(() => {
      api<SignIn>("sharing/github_login_status")
        .then((s) => {
          if (!active || s.status === "pending") return;
          setSignIn(s);
          if (s.status === "done") signedIn.current?.(s.login || "");
        })
        .catch((e) => {
          if (active) setError(String(e));
        });
    }, 2000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [signIn.status]);
  async function start() {
    setBusy(true);
    setError("");
    try {
      const s = await api<SignIn>("sharing/github_login_start", {});
      setSignIn(s);
      // The click is recent enough for the clipboard on most browsers; the
      // copy button covers the rest.
      if (s.user_code) copy(s.user_code);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  async function cancel() {
    setBusy(true);
    try {
      await api("sharing/github_login_cancel", {});
      setSignIn({ status: "none" });
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  const host = (url: string) => {
    try {
      return new URL(url).host + new URL(url).pathname;
    } catch {
      return url;
    }
  };
  return (
    <div className="github-signin">
      {signIn.status === "pending" ? (
        <div className="github-signin-pending" role="status">
          <p>
            Enter this code at GitHub to sign in:{" "}
            <strong className="github-signin-code">{signIn.user_code}</strong>
            <button
              type="button"
              className="ghost"
              disabled={disabled}
              onClick={() => copy(signIn.user_code || "")}
              aria-label="Copy code"
            >
              {copied ? <Check size={14} /> : <Copy size={14} />}
              {copied ? "Copied" : "Copy"}
            </button>
          </p>
          <div className="admin-actions">
            <a
              className="button primary"
              href={signIn.verification_uri}
              target="_blank"
              rel="noreferrer"
            >
              <ExternalLink size={14} />
              Open {host(signIn.verification_uri || "")}
            </a>
            <button type="button" disabled={busy || disabled} onClick={cancel}>
              Cancel
            </button>
          </div>
          <p className="muted">
            Waiting for GitHub… this page updates by itself once you have
            authorised Warden
            {signIn.expires_at
              ? ` (the code expires ${new Date(signIn.expires_at * 1000).toLocaleTimeString()})`
              : ""}
            .
          </p>
        </div>
      ) : (
        <div className="admin-actions">
          <button type="button" disabled={busy || disabled} onClick={start}>
            <GitBranch size={14} />
            {connected ? "Refresh sign-in" : "Sign in with GitHub"}
          </button>
          {children}
        </div>
      )}
      {signIn.status === "done" && (
        <p className="admin-notice">Signed in to GitHub as {signIn.login}.</p>
      )}
      {(error || signIn.status === "failed") && (
        <p role="alert" className="error">
          {error || signIn.error}
        </p>
      )}
    </div>
  );
}
