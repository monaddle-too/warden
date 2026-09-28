import { configureReferences } from "../references";
import { AgentConsent } from "./ExternalAgents";
import { useCallback, useEffect, useState } from "react";
import { ChevronDown, Shield } from "lucide-react";
import {
  GoogleLogin,
  type AuthState,
  type BrowserSession,
} from "./GoogleLogin";
import { ChatShell } from "./ChatShell";
import { EmailLogin } from "./EmailLogin";
import { OrganizationAdmin } from "./OrganizationAdmin";
import { PersonalSettings, PasskeyInvitation } from "./PersonalSettings";
import { remoteSession } from "../api";
import { documentTitle, signedOutHeading } from "../instance";
export function AuthRoot() {
  const [auth, setAuth] = useState<AuthState>();
  const [error, setError] = useState("");
  const accept = useCallback((session: BrowserSession) => {
    remoteSession(session.csrf, session.user);
    configureReferences(
      session.mode === "email" && session.organizationId
        ? `${session.user?.sub}:${session.organizationId}`
        : "",
      session.csrf,
    );
    setAuth({ enabled: true, ...session });
    const next = new URLSearchParams(location.search).get("next");
    if (next && !session.offerPasskey) {
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
      configureReferences("");
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
    configureReferences("");
    await load();
  }
  async function selectOrganization(id: string) {
    if (!auth?.csrf) return;
    const response = await fetch("/auth/organizations/select", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Warden-CSRF": auth.csrf,
      },
      body: JSON.stringify({ id }),
    });
    if (!response.ok) {
      setError("That organization is unavailable.");
      return;
    }
    sessionStorage.removeItem("warden-selected-chat");
    location.assign(
      /^\/(documents|designs)(\/|$)/.test(location.pathname)
        ? "/documents"
        : location.pathname.startsWith("/shared-conversations")
          ? "/shared-conversations"
          : "/",
    );
  }
  // Which Warden is asking: a loopback owner install names itself and its
  // build here and in the tab's title, so an install opened next to another
  // one (or through another Warden's preview proxy) is recognisable before
  // anyone signs in. A public install reports no instance (edge/owner.go).
  const signedOut = !auth || (auth.enabled && !auth.user);
  const instance = auth?.instance;
  useEffect(() => {
    if (signedOut) document.title = documentTitle("Warden — Sign in", instance);
  }, [signedOut, instance?.name, instance?.version]);
  if (signedOut)
    return (
      <div className="signin">
        <Shield size={36} />
        <h1>{signedOutHeading(instance)}</h1>
        <p>Sign in to access your agents and private previews.</p>
        {auth?.mode === "email" ? (
          <EmailLogin onSignedIn={() => void load()} />
        ) : (
          auth && <GoogleLogin auth={auth} onSession={accept} onRetry={load} />
        )}
        <p role="alert">{error}</p>
      </div>
    );
  if (auth.mode === "email" && auth.offerPasskey && auth.csrf)
    return <PasskeyInvitation csrf={auth.csrf} onDone={() => void load()} />;
  if (auth.mode === "email" && auth.csrf && location.pathname === "/connect")
    return (
      <AgentConsent
        csrf={auth.csrf}
        organizations={auth.organizations || []}
        selected={auth.organizationId}
      />
    );
  const currentOrganization = auth.organizations?.find(
    (org) => org.id === auth.organizationId,
  );
  const organizationAdmin =
    auth.mode === "email" &&
    (auth.fullAdmin || auth.organizationRole === "admin") ? (
      <OrganizationAdmin
        organization={currentOrganization}
        organizations={
          auth.fullAdmin
            ? auth.organizations || []
            : currentOrganization
              ? [currentOrganization]
              : []
        }
        fullAdmin={!!auth.fullAdmin}
        onChanged={() => void load()}
        onSelect={(id) => void selectOrganization(id)}
      />
    ) : undefined;
  if (auth.mode === "email" && !currentOrganization)
    return (
      <div className="signin">
        <Shield size={36} />
        <h1>Choose an organization</h1>
        <select
          aria-label="Organization"
          defaultValue=""
          onChange={(event) => void selectOrganization(event.target.value)}
        >
          <option value="" disabled>
            Choose an organization
          </option>
          {auth.organizations?.map((org) => (
            <option value={org.id} key={org.id}>
              {org.name}
            </option>
          ))}
        </select>
        {organizationAdmin}
        <button className="ghost" onClick={logout}>
          Sign out
        </button>
        {error && <p role="alert">{error}</p>}
      </div>
    );
  return (
    <ChatShell
      instance={instance}
      canConnectGoogle={
        auth.mode !== "email" && (!auth.enabled || auth.user?.role === "admin")
      }
      admin={!auth.enabled || auth.user?.role === "admin"}
      organizationAdmin={organizationAdmin}
      personalSettings={
        auth.mode === "email" && auth.csrf ? (
          <PersonalSettings csrf={auth.csrf} onChanged={() => void load()} />
        ) : undefined
      }
      platformAdmin={!!auth.fullAdmin}
      docsEnabled={auth.mode === "email"}
      signIn={auth.enabled}
      accountName={auth.user?.name || auth.user?.email}
      organizationSwitcher={
        auth.mode === "email" ? (
          <label className="warden-organization">
            Current organization
            <select
              aria-label="Current organization"
              value={auth.organizationId || ""}
              onChange={(event) => void selectOrganization(event.target.value)}
            >
              {auth.organizations?.map((org) => (
                <option value={org.id} key={org.id}>
                  {org.name}
                </option>
              ))}
            </select>
            <ChevronDown size={14} aria-hidden="true" />
          </label>
        ) : undefined
      }
      account={
        auth.enabled ? (
          <div
            className={`warden-account${auth.mode === "email" ? " warden-cloud-account" : ""}`}
          >
            <div className="warden-account-person">
              <span className="warden-account-avatar" aria-hidden="true">
                {(auth.user?.email || "?").slice(0, 1)}
              </span>
              <small title={auth.user?.email}>
                {auth.user?.name || auth.user?.email}
              </small>
            </div>
            {auth.fullAdmin && (
              <span className="warden-account-role">
                Platform administrator
              </span>
            )}
            <div className="warden-account-actions">
              <button className="ghost" onClick={logout}>
                Sign out
              </button>
            </div>
          </div>
        ) : undefined
      }
    />
  );
}
