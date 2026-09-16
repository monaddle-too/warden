import {
  useEffect,
  useImperativeHandle,
  useRef,
  useState,
  type ReactNode,
  type Ref,
} from "react";
import { GitFork } from "lucide-react";
import { api } from "../api";

type Repo = { id: number; full_name: string; private?: boolean };
type GitHubStatus = { configured?: boolean; appSlug?: string };
export type RepositorySharingHandle = { open: () => void };
export function RepositorySharing({
  ref,
  chatID,
  trigger,
}: {
  ref?: Ref<RepositorySharingHandle>;
  chatID?: string;
  trigger?: ((open: () => void) => ReactNode) | null;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [open, setOpen] = useState(false);
  // providers.github decides whether the repository section exists at all
  // and which GitHub App (if any) the installation link points at.
  const [github, setGitHub] = useState<GitHubStatus>({});
  useEffect(() => {
    let active = true;
    api<{ github?: GitHubStatus }>("sharing/status")
      .then((s) => {
        if (active) setGitHub(s.github || {});
      })
      .catch(() => {
        /* Keep the section visible; the dialog reports errors on use. */
      });
    return () => {
      active = false;
    };
  }, []);
  const [repos, setRepos] = useState<Repo[]>([]);
  const [selected, setSelected] = useState<string[]>([]);
  const [owner, setOwner] = useState("");
  const [installation, setInstallation] = useState<number>();
  const [page, setPage] = useState<number | null>(null);
  const [query, setQuery] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    if (open) dialog.current?.showModal();
  }, [open]);
  async function load(next = 1) {
    if (!chatID) return;
    setBusy(true);
    setError("");
    try {
      if (next === 1) {
        const shared = await api<{ owner: string; repositories: Repo[] }>(
          `sharing/github_list?chatID=${encodeURIComponent(chatID)}`,
        );
        setSelected(shared.repositories.map((r) => r.full_name.toLowerCase()));
        setRepos(shared.repositories);
        setOwner(shared.owner);
      }
      const result = await api<{
        owner: string;
        installation_id: number;
        repositories: Repo[];
        next_page: number | null;
      }>(`sharing/github_repositories?page=${next}`);
      setRepos((old) => [
        ...new Map(
          [...old, ...result.repositories].map((r) => [
            r.full_name.toLowerCase(),
            r,
          ]),
        ).values(),
      ]);
      setOwner(result.owner);
      setInstallation(result.installation_id);
      setPage(result.next_page);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  async function save() {
    setBusy(true);
    setError("");
    try {
      await api("sharing/github_select", { chatID, repositories: selected });
      setOpen(false);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  const show = () => {
    setOpen(true);
    void load();
  };
  useImperativeHandle(ref, () => ({ open: show }));
  // providers.github absent: no repository section at all (after the hooks).
  if (github.configured === false) return null;
  const install = installation
    ? `https://github.com/settings/installations/${installation}`
    : github.appSlug
      ? `https://github.com/apps/${github.appSlug}/installations/new`
      : "";
  return (
    <>
      {trigger === null ? null : trigger ? (
        trigger(show)
      ) : (
        <button className="chip" disabled={!chatID} onClick={show}>
          <GitFork size={13} />
          <span>Repositories</span>
        </button>
      )}
      {open && (
        <dialog
          ref={dialog}
          className="sharing-dialog"
          aria-labelledby="repositories-title"
          onCancel={() => setOpen(false)}
        >
          <header>
            <h2 id="repositories-title">Share GitHub repositories</h2>
            <button onClick={() => setOpen(false)}>Close</button>
          </header>
          <p>
            Selected repositories stay readable by every chat in this workspace
            until you remove them. Writing and pushing require separate
            permission.
          </p>
          {owner && (
            <p>
              Connected GitHub account: <strong>{owner}</strong>
            </p>
          )}
          <p className="muted">
            Shows repositories you own that are connected to Warden’s GitHub
            App.
          </p>
          {install && (
            <a href={install} target="_blank" rel="noreferrer">
              Manage connected repositories on GitHub
            </a>
          )}
          <button disabled={busy} onClick={() => void load()}>
            Refresh repositories
          </button>
          {error && (
            <p role="alert" className="error">
              {error}
            </p>
          )}
          <label>
            Find a repository
            <input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Filter loaded repositories"
            />
          </label>
          <div className="sharing-files">
            {repos
              .filter((r) =>
                r.full_name.toLowerCase().includes(query.toLowerCase()),
              )
              .map((r) => {
                const name = r.full_name.toLowerCase();
                return (
                  <label key={r.id}>
                    <input
                      type="checkbox"
                      disabled={busy}
                      checked={selected.includes(name)}
                      onChange={(e) =>
                        setSelected((old) =>
                          e.target.checked
                            ? [...old, name]
                            : old.filter((n) => n !== name),
                        )
                      }
                    />
                    <span>
                      {r.full_name}
                      {r.private ? " · Private" : ""}
                    </span>
                  </label>
                );
              })}
            {!busy && !repos.length && !error && (
              <p>No connected repositories found.</p>
            )}
          </div>
          {page && (
            <button disabled={busy} onClick={() => void load(page)}>
              Load more repositories
            </button>
          )}
          <footer>
            <span>{selected.length} selected · Read-only · Until removed</span>
            <button
              className="primary"
              disabled={busy || selected.length > 100 || !owner}
              onClick={() => void save()}
            >
              {busy ? "Loading…" : "Save repository access"}
            </button>
          </footer>
        </dialog>
      )}
    </>
  );
}
