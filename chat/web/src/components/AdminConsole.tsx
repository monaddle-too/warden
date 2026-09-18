import { useCallback, useEffect, useState } from "react";
import {
  FileText,
  GitBranch,
  Globe,
  KeyRound,
  RefreshCw,
  ShieldOff,
  Users,
} from "lucide-react";
import { api } from "../api";
import { ClusterView } from "./ClusterView";
import { GitHubSignIn } from "./GitHubSignIn";
import { SpendView } from "./SpendView";

type LoginRecord = {
  email: string;
  sub: string;
  role: string;
  hosted_domain?: string;
  first_login: string;
  last_login: string;
  logins: number;
};
type Status = {
  configured: boolean;
  connected: boolean;
  can_write: boolean;
  github?: {
    configured: boolean;
    connected: boolean;
    owner: string;
    appSlug?: string;
    mode?: string;
    login?: string;
    scopes?: string[];
    obtained?: number;
    disconnectable?: boolean;
  };
};
type File = { id: string; name: string; blocked: boolean };
type Repo = { id: number; full_name: string; private?: boolean };
type Blocked = { id: string; name: string; blocked_at: number };
type Egress = { mode: "restricted" | "open"; source: "config" | "console" };

const when = (value: string | number) =>
  new Date(typeof value === "number" ? value * 1000 : value).toLocaleString();

// signIn: the edge authenticates people (server mode). Without it the
// install has one owner and no sign-in ledger, so that section is omitted.
export function AdminConsole({ signIn = true }: { signIn?: boolean }) {
  const [users, setUsers] = useState<LoginRecord[]>([]);
  const [persistent, setPersistent] = useState(true);
  const [status, setStatus] = useState<Status>();
  const [files, setFiles] = useState<File[]>([]);
  const [page, setPage] = useState("");
  const [repos, setRepos] = useState<Repo[]>([]);
  const [repoPage, setRepoPage] = useState<number | null>(null);
  const [repoError, setRepoError] = useState("");
  const [blocked, setBlocked] = useState<Blocked[]>([]);
  const [egress, setEgress] = useState<Egress>();
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState<string | boolean>(false);
  const [loading, setLoading] = useState(true);

  const loadFiles = useCallback(async (next = "") => {
    const r = await api<{ files: File[]; nextPageToken?: string }>(
      "sharing/files" + (next ? "?page=" + encodeURIComponent(next) : ""),
    );
    setFiles((old) => (next ? [...old, ...r.files] : r.files));
    setPage(r.nextPageToken || "");
  }, []);
  const loadRepos = useCallback(async (next = 1) => {
    setRepoError("");
    try {
      const r = await api<{ repositories: Repo[]; next_page: number | null }>(
        `sharing/github_repositories?page=${next}`,
      );
      setRepos((old) =>
        next === 1 ? r.repositories : [...old, ...r.repositories],
      );
      setRepoPage(r.next_page);
    } catch (e) {
      setRepoError(String(e));
    }
  }, []);
  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const [u, s, b, e] = await Promise.all([
        signIn
          ? api<{ persistent: boolean; users: LoginRecord[] }>("admin/users")
          : Promise.resolve({ persistent: true, users: [] }),
        api<Status>("sharing/status"),
        api<{ documents: Blocked[] }>("sharing/blocked"),
        api<Egress>("sharing/egress").catch(() => undefined),
      ]);
      setUsers(u.users);
      setPersistent(u.persistent);
      setStatus(s);
      setBlocked(b.documents);
      setEgress(e);
      await Promise.all([
        s.connected ? loadFiles() : Promise.resolve(),
        s.github?.connected ? loadRepos() : Promise.resolve(),
      ]);
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, [loadFiles, loadRepos, signIn]);
  useEffect(() => {
    void load();
  }, [load]);

  // Disconnect forgets the stored credential on the policy service and
  // revokes everything it backed; the account can be connected again.
  async function disconnect(provider: "google" | "github") {
    const what = provider === "google" ? "Google Docs" : "GitHub";
    const extra =
      provider === "google"
        ? " Every document grant is revoked."
        : " Every repository selection is dropped. The token itself stays valid at GitHub until you revoke it under Settings → Applications.";
    if (!window.confirm(`Disconnect ${what} from Warden?${extra}`)) return;
    setBusy(provider);
    setError("");
    setNotice("");
    try {
      await api("sharing/disconnect", { provider });
      setNotice(`${what} disconnected.`);
      if (provider === "google") {
        setFiles([]);
        setPage("");
      } else {
        setRepos([]);
        setRepoPage(null);
      }
      await load();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  // The egress switch applies to every sandbox at once, running ones
  // included, and persists on the policy service until changed again.
  async function setEgressMode(mode: Egress["mode"]) {
    if (egress?.mode === mode) return;
    setBusy("egress");
    setError("");
    setNotice("");
    try {
      const r = await api<Egress>("sharing/egress_set", { mode });
      setEgress(r);
      setNotice(
        mode === "open"
          ? "Sandboxes may now reach any public website. Credentials are still injected only for approved requests."
          : "Sandboxes are back to the restricted destination list.",
      );
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  async function connectGoogle() {
    const popup = window.open(
      "about:blank",
      "warden-google",
      "width=600,height=760",
    );
    setError("");
    try {
      const r = await api<{ authorization_url: string }>("sharing/connect", {});
      if (popup) popup.location.href = r.authorization_url;
      else setError("Allow popups for Warden, then try Google sign-in again.");
    } catch (e) {
      popup?.close();
      setError(String(e));
    }
  }

  async function tag(id: string, name: string, block: boolean) {
    setBusy(id);
    setError("");
    setNotice("");
    try {
      const r = await api<{ revoked: string[] }>(
        block ? "sharing/block" : "sharing/unblock",
        block ? { id, name } : { id },
      );
      const b = await api<{ documents: Blocked[] }>("sharing/blocked");
      setBlocked(b.documents);
      setFiles((old) =>
        old.map((f) => (f.id === id ? { ...f, blocked: block } : f)),
      );
      if (block && r.revoked.length)
        setNotice(
          `Revoked ${r.revoked.length} active sharing grant${r.revoked.length === 1 ? "" : "s"} for “${name}”.`,
        );
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  const owners = users.filter((u) => u.role === "admin");
  const google = status?.connected ? status : undefined;
  const github = status?.github?.connected ? status.github : undefined;
  const accounts = (
    <>
      {status?.configured && (
        <details open className="admin-account">
          <summary>
            <FileText size={14} />
            Google Docs
            <small>
              {google
                ? google.can_write
                  ? "connected · read, edit and create"
                  : "connected · read only"
                : "not connected"}
            </small>
          </summary>
          {google ? (
            <>
              <p className="muted">
                Documents created September 9, 2026 or later. Tagged documents
                can never be shared with a conversation.
              </p>
              <ul className="admin-list">
                {files.map((f) => (
                  <li key={f.id} className={f.blocked ? "blocked" : ""}>
                    <span>{f.name}</span>
                    <label className="admin-toggle">
                      <input
                        type="checkbox"
                        checked={f.blocked}
                        disabled={busy === f.id}
                        onChange={(e) =>
                          void tag(f.id, f.name, e.target.checked)
                        }
                      />
                      Unsharable with AI
                    </label>
                  </li>
                ))}
                {!files.length && (
                  <li className="muted">No Google documents found.</li>
                )}
              </ul>
              <div className="admin-actions">
                {page && (
                  <button
                    disabled={!!busy}
                    onClick={() => {
                      setBusy(true);
                      void loadFiles(page)
                        .catch((e) => setError(String(e)))
                        .finally(() => setBusy(false));
                    }}
                  >
                    Load more documents
                  </button>
                )}
                <button
                  className="danger"
                  disabled={!!busy}
                  onClick={() => void disconnect("google")}
                >
                  Disconnect Google
                </button>
              </div>
            </>
          ) : (
            <div className="admin-actions">
              <p className="muted">
                Connect a Google account to share documents with conversations.
              </p>
              <button disabled={!!busy} onClick={() => void connectGoogle()}>
                Sign in with Google
              </button>
            </div>
          )}
        </details>
      )}
      {status?.github?.configured && (
        <details open className="admin-account">
          <summary>
            <GitBranch size={14} />
            {status.github.mode === "user" ? "GitHub" : "GitHub App"}
            <small>
              {github
                ? github.login
                  ? `connected as ${github.login}`
                  : github.owner
                : "not connected"}
            </small>
          </summary>
          {github ? (
            <>
              <p className="muted">
                {github.mode === "user"
                  ? `Signed in with a user token${github.scopes?.length ? ` (${github.scopes.join(", ")})` : ""}${github.obtained ? `, stored ${when(github.obtained)}` : ""}. Repositories the account can share, read only.`
                  : "Repositories the installation can share, read only."}
              </p>
              {repoError && (
                <p role="alert" className="error">
                  {repoError}
                </p>
              )}
              <ul className="admin-list">
                {repos.map((r) => (
                  <li key={r.id}>
                    <span>{r.full_name}</span>
                    <small>{r.private ? "private" : "public"}</small>
                  </li>
                ))}
                {!repos.length && !repoError && (
                  <li className="muted">No repositories available.</li>
                )}
              </ul>
              <div className="admin-actions">
                {repoPage && (
                  <button onClick={() => void loadRepos(repoPage)}>
                    Load more repositories
                  </button>
                )}
                {github.disconnectable && github.mode !== "user" && (
                  <button
                    className="danger"
                    disabled={!!busy}
                    onClick={() => void disconnect("github")}
                  >
                    Disconnect GitHub
                  </button>
                )}
              </div>
              {github.mode === "user" && (
                // A rejected or expired token fails closed ("Refresh the
                // GitHub sign-in…"); a fresh device-flow sign-in replaces it.
                <GitHubSignIn
                  connected
                  disabled={!!busy}
                  onSignedIn={() => void load()}
                >
                  {github.disconnectable && (
                    <button
                      className="danger"
                      disabled={!!busy}
                      onClick={() => void disconnect("github")}
                    >
                      Disconnect GitHub
                    </button>
                  )}
                </GitHubSignIn>
              )}
            </>
          ) : status.github.mode === "user" || !status.github.appSlug ? (
            <>
              <p className="muted">
                Sign in with the GitHub account whose repositories conversations
                may share. Warden asks for the classic <code>repo</code> and{" "}
                <code>read:org</code> scopes and keeps the token on this
                machine.
              </p>
              <GitHubSignIn
                connected={false}
                disabled={!!busy}
                onSignedIn={() => void load()}
              />
              <p className="muted">
                Or from a terminal: <code>warden login github</code>
              </p>
            </>
          ) : (
            <p className="muted">
              Install the GitHub App to connect:{" "}
              <a
                href={`https://github.com/apps/${status.github.appSlug}/installations/new`}
                target="_blank"
                rel="noreferrer"
              >
                github.com/apps/{status.github.appSlug}
              </a>
            </p>
          )}
        </details>
      )}
      {!status?.configured && !status?.github?.configured && !loading && (
        <p className="muted">
          No providers are configured. Add Google or GitHub to warden.json.
        </p>
      )}
    </>
  );

  return (
    <div className="admin-console">
      <header className="chat-header">
        <h1>Admin console</h1>
        <button
          className="ghost"
          disabled={loading}
          onClick={() => void load()}
        >
          <RefreshCw size={15} />
          Refresh
        </button>
      </header>
      <div className="admin-body">
        {error && (
          <p role="alert" className="error">
            {error}
          </p>
        )}
        {notice && <p className="admin-notice">{notice}</p>}
        <section aria-labelledby="admin-accounts">
          <h2 id="admin-accounts">
            <KeyRound size={16} />
            Connected accounts
          </h2>
          {loading && !status && <p className="muted">Loading…</p>}
          {accounts}
        </section>
        {egress && (
          <section aria-labelledby="admin-network">
            <h2 id="admin-network">
              <Globe size={16} />
              Network access
            </h2>
            <p className="muted">
              Every sandbox talks to the internet only through its inspecting
              gateway, which injects credentials solely for approved requests.
              This chooses what else the gateway lets through. It applies to all
              sandboxes immediately and{" "}
              {egress.source === "console"
                ? "was last set here, overriding warden.json"
                : "currently comes from warden.json"}
              .
            </p>
            <div
              className="admin-choices"
              role="radiogroup"
              aria-label="Network access"
            >
              {(
                [
                  {
                    mode: "restricted",
                    title: "Restricted",
                    text: "The AI providers, package registries and the policy template's destination list. GitHub, Google Docs and Figma only through a grant. Everything else is refused.",
                  },
                  {
                    mode: "open",
                    title: "Open",
                    text: "Any public HTTP or HTTPS website. Granted GitHub, Google Docs and Figma requests are brokered as before; ungranted ones go through anonymously, with no credential attached.",
                  },
                ] as { mode: Egress["mode"]; title: string; text: string }[]
              ).map((choice) => (
                <button
                  key={choice.mode}
                  role="radio"
                  aria-checked={egress.mode === choice.mode}
                  className={
                    "admin-choice" +
                    (egress.mode === choice.mode ? " selected" : "")
                  }
                  disabled={busy === "egress"}
                  onClick={() => void setEgressMode(choice.mode)}
                >
                  <strong>{choice.title}</strong>
                  <span>{choice.text}</span>
                </button>
              ))}
            </div>
          </section>
        )}
        {signIn && !persistent && (
          <p className="admin-notice">
            The sign-in ledger is not persisted; it lists sign-ins since the
            edge last started.
          </p>
        )}
        {signIn && (
          <section aria-labelledby="admin-users">
            <h2 id="admin-users">
              <Users size={16} />
              Signed-in users
            </h2>
            {loading && !users.length && <p className="muted">Loading…</p>}
            {!loading && !users.length && (
              <p className="muted">No sign-ins recorded yet.</p>
            )}
            {users.map((u) => (
              <article key={u.email} className="admin-user">
                <header>
                  <div>
                    <strong>{u.email}</strong>
                    <span className={"admin-role " + u.role}>
                      {u.role === "admin" ? "Owner" : "Demo"}
                    </span>
                    {u.hosted_domain && <small>{u.hosted_domain}</small>}
                  </div>
                  <small>
                    {u.logins} sign-in{u.logins === 1 ? "" : "s"} · first{" "}
                    {when(u.first_login)} · last {when(u.last_login)}
                  </small>
                </header>
              </article>
            ))}
            {!loading && !owners.length && (google || github) && (
              <p className="muted">
                Accounts are connected but the owner has not signed in since the
                ledger started.
              </p>
            )}
          </section>
        )}
        <SpendView />
        <ClusterView />
        <section aria-labelledby="admin-blocked">
          <h2 id="admin-blocked">
            <ShieldOff size={16} />
            Documents tagged unsharable with AI
          </h2>
          {!blocked.length && <p className="muted">No documents are tagged.</p>}
          <ul className="admin-list">
            {blocked.map((d) => (
              <li key={d.id} className="blocked">
                <span>
                  {d.name || d.id}
                  <small> · tagged {when(d.blocked_at)}</small>
                </span>
                <button
                  disabled={busy === d.id}
                  onClick={() => void tag(d.id, d.name, false)}
                >
                  Allow sharing
                </button>
              </li>
            ))}
          </ul>
        </section>
      </div>
    </div>
  );
}
