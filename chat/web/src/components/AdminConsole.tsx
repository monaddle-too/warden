import { useCallback, useEffect, useState } from "react";
import { FileText, GitBranch, RefreshCw, ShieldOff, Users } from "lucide-react";
import { api } from "../api";

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
  github?: { connected: boolean; owner: string };
};
type File = { id: string; name: string; blocked: boolean };
type Repo = { id: number; full_name: string; private?: boolean };
type Blocked = { id: string; name: string; blocked_at: number };

const when = (value: string | number) =>
  new Date(typeof value === "number" ? value * 1000 : value).toLocaleString();

export function AdminConsole() {
  const [users, setUsers] = useState<LoginRecord[]>([]);
  const [persistent, setPersistent] = useState(true);
  const [status, setStatus] = useState<Status>();
  const [files, setFiles] = useState<File[]>([]);
  const [page, setPage] = useState("");
  const [repos, setRepos] = useState<Repo[]>([]);
  const [repoPage, setRepoPage] = useState<number | null>(null);
  const [repoError, setRepoError] = useState("");
  const [blocked, setBlocked] = useState<Blocked[]>([]);
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
      const [u, s, b] = await Promise.all([
        api<{ persistent: boolean; users: LoginRecord[] }>("admin/users"),
        api<Status>("sharing/status"),
        api<{ documents: Blocked[] }>("sharing/blocked"),
      ]);
      setUsers(u.users);
      setPersistent(u.persistent);
      setStatus(s);
      setBlocked(b.documents);
      await Promise.all([
        s.connected ? loadFiles() : Promise.resolve(),
        s.github?.connected ? loadRepos() : Promise.resolve(),
      ]);
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, [loadFiles, loadRepos]);
  useEffect(() => {
    void load();
  }, [load]);

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
  const accounts = (u: LoginRecord) => {
    if (u.role !== "admin")
      return (
        <p className="muted">
          No connected accounts. Only the demo owner can connect accounts.
        </p>
      );
    if (!google && !github)
      return <p className="muted">No accounts connected.</p>;
    return (
      <>
        {google && (
          <details open className="admin-account">
            <summary>
              <FileText size={14} />
              Google Docs
              <small>
                {google.can_write ? "read, edit and create" : "read only"}
              </small>
            </summary>
            <p className="muted">
              Documents created September 9, 2026 or later. Tagged documents can
              never be shared with a conversation.
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
                      onChange={(e) => void tag(f.id, f.name, e.target.checked)}
                    />
                    Unsharable with AI
                  </label>
                </li>
              ))}
              {!files.length && (
                <li className="muted">No Google documents found.</li>
              )}
            </ul>
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
          </details>
        )}
        {github && (
          <details open className="admin-account">
            <summary>
              <GitBranch size={14} />
              GitHub App
              <small>{github.owner}</small>
            </summary>
            <p className="muted">
              Repositories the installation can share, read only.
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
            {repoPage && (
              <button onClick={() => void loadRepos(repoPage)}>
                Load more repositories
              </button>
            )}
          </details>
        )}
      </>
    );
  };

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
        {!persistent && (
          <p className="admin-notice">
            The sign-in ledger is not persisted; it lists sign-ins since the
            edge last started.
          </p>
        )}
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
              <h3>Connected accounts</h3>
              {accounts(u)}
            </article>
          ))}
          {!loading && !owners.length && (google || github) && (
            <p className="muted">
              Accounts are connected but the owner has not signed in since the
              ledger started, so they are not listed under a user yet.
            </p>
          )}
        </section>
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
