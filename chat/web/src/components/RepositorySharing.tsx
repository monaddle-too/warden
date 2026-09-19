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
import { GitHubSignIn } from "./GitHubSignIn";

type Repo = {
  id: number;
  full_name: string;
  private?: boolean;
  access?: string[];
};
// Read categories a shared repository can expose; the policy service
// decides which GitHub operations each covers. Metadata always comes along.
const CATEGORIES: { id: string; label: string; hint: string }[] = [
  { id: "contents", label: "Code", hint: "files, branches, commits, clone" },
  {
    id: "issues",
    label: "Issues",
    hint: "issues, comments, labels, milestones",
  },
  { id: "pull_requests", label: "Pull requests", hint: "PRs, files, reviews" },
];
const ALL = CATEGORIES.map((c) => c.id);
type GitHubStatus = {
  configured?: boolean;
  appSlug?: string;
  mode?: string;
  connected?: boolean;
};
export type RepositorySharingHandle = { open: () => void };
export function RepositorySharing({
  ref,
  chatID,
  trigger,
  admin,
}: {
  ref?: Ref<RepositorySharingHandle>;
  chatID?: string;
  trigger?: ((open: () => void) => ReactNode) | null;
  // The owner can refresh the GitHub sign-in from here when it has lapsed.
  admin?: boolean;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [open, setOpen] = useState(false);
  // providers.github decides whether the repository section exists at all
  // and which GitHub App (if any) the installation link points at.
  const [github, setGitHub] = useState<GitHubStatus>({});
  const [statusTick, setStatusTick] = useState(0);
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
  }, [statusTick]);
  const [repos, setRepos] = useState<Repo[]>([]);
  const [selected, setSelected] = useState<string[]>([]);
  // name -> chosen read categories (every category unless changed)
  const [access, setAccess] = useState<Record<string, string[]>>({});
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
        setAccess(
          Object.fromEntries(
            shared.repositories.map((r) => [
              r.full_name.toLowerCase(),
              r.access?.length ? r.access : ALL,
            ]),
          ),
        );
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
      await api("sharing/github_select", {
        chatID,
        repositories: selected,
        access: Object.fromEntries(
          selected.map((name) => [name, access[name] || ALL]),
        ),
      });
      setOpen(false);
      // The workspace panel lists the shares; refresh it now rather than
      // on its next poll.
      window.dispatchEvent(new Event("warden-refresh-state"));
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
            until you remove them, limited to the categories you tick. Writing
            and pushing always require separate permission.
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
          {admin &&
            github.mode === "user" &&
            (!github.connected ||
              error.includes("Refresh the GitHub sign-in")) && (
              <GitHubSignIn
                connected={!!github.connected}
                disabled={busy}
                onSignedIn={() => {
                  setStatusTick((n) => n + 1);
                  void load();
                }}
              />
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
                const on = selected.includes(name);
                const chosen = access[name] || ALL;
                return (
                  <div key={r.id} className="sharing-repo">
                    <label>
                      <input
                        type="checkbox"
                        disabled={busy}
                        checked={on}
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
                    {on && (
                      <div className="sharing-categories">
                        {CATEGORIES.map((c) => (
                          <label key={c.id} title={c.hint}>
                            <input
                              type="checkbox"
                              disabled={busy}
                              checked={chosen.includes(c.id)}
                              onChange={(e) =>
                                setAccess((old) => ({
                                  ...old,
                                  [name]: e.target.checked
                                    ? ALL.filter(
                                        (id) =>
                                          id === c.id || chosen.includes(id),
                                      )
                                    : chosen.filter((id) => id !== c.id),
                                }))
                              }
                            />
                            <span>{c.label}</span>
                          </label>
                        ))}
                      </div>
                    )}
                  </div>
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
              disabled={
                busy ||
                selected.length > 100 ||
                !owner ||
                selected.some((n) => (access[n] || ALL).length === 0)
              }
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
