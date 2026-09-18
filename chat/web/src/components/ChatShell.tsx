// Adapted from Panta ChatShell.tsx. Warden owns chats and workspace selection.
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type ReactNode,
  type FormEvent,
} from "react";
import {
  Archive,
  ArchiveRestore,
  Box,
  Download,
  FileDiff,
  FileText,
  GitPullRequest,
  History,
  MoreHorizontal,
  NotebookPen,
  PanelRight,
  Pencil,
  Plus,
  RefreshCw,
  Search,
  Shield,
  ShieldCheck,
  TextSearch,
  Timer,
} from "lucide-react";
import type { Chat, Environment, Resources, State } from "../types";
import { api, signedIn, subscribe } from "../api";
import { plural, providerName } from "../export";
import {
  PullRequestReview,
  type PullRequestReviewHandle,
  type PullRequestState,
} from "./PullRequestReview";
import {
  RepositorySharing,
  type RepositorySharingHandle,
} from "./RepositorySharing";
import {
  DocumentSharing,
  type DocumentSharingHandle,
  type DocumentSharingState,
} from "./DocumentSharing";
import {
  DocumentReview,
  type DocumentReviewHandle,
  type DocumentReviewState,
} from "./DocumentReview";
import { Previews } from "./Previews";
import { Conversation, type RequestCard } from "./Conversation";
import { ModelSelect } from "./ModelSelect";
import { SizeSelect, sameSize } from "./SizeSelect";
import { AdminConsole } from "./AdminConsole";
import { chatStatusLabel } from "../stages";
import { WorkspacePanel } from "./WorkspacePanel";
import { ExportDialog } from "./ExportDialog";
import { RewindDialog } from "./RewindDialog";
import { SessionDiff } from "./SessionDiff";
import { InstructionsDialog } from "./InstructionsDialog";
import { SearchPalette } from "./SearchPalette";
import { modifierKey, type FindRequest } from "./FindBar";

export function ChatShell({
  account,
  canConnectGoogle = true,
  admin = false,
  signIn = false,
}: {
  account?: ReactNode;
  canConnectGoogle?: boolean;
  admin?: boolean;
  signIn?: boolean;
}) {
  const [state, setState] = useState<State>({ version: 1, chats: [] });
  const [live, setLive] = useState(false);
  const [selected, setSelected] = useState(
    () =>
      new URLSearchParams(location.search).get("chat") ||
      sessionStorage.getItem("warden-selected-chat") ||
      "",
  );
  const [creating, setCreating] = useState(false);
  const [adminOpen, setAdminOpen] = useState(false);
  const [archived, setArchived] = useState(false);
  const [title, setTitle] = useState("");
  const [shared, setShared] = useState("");
  const [provider, setProvider] = useState("codex");
  const [model, setModel] = useState("");
  const [repository, setRepository] = useState("");
  // The fresh workspace's size; null means the runner's default.
  const [size, setSize] = useState<Resources | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [workspaceState, setWorkspaceState] = useState("");
  const [menuOpen, setMenuOpen] = useState(false);
  const [exporting, setExporting] = useState(false);
  // The rewind chooser (the message it opens on, "" for the last) and the
  // session diff (rewind.ts).
  const [rewinding, setRewinding] = useState<string | null>(null);
  const [changesOpen, setChangesOpen] = useState(false);
  const [instructionsOpen, setInstructionsOpen] = useState(false);
  const [searching, setSearching] = useState(false);
  // The find bar's latest request; a new object each time so the same
  // query can be asked for again.
  const [find, setFind] = useState<FindRequest>();
  const [previewCount, setPreviewCount] = useState(0);
  const [workspaces, setWorkspaces] = useState<Environment[]>([]);
  const [workspaceOpen, setWorkspaceOpen] = useState(
    () => sessionStorage.getItem("warden-workspace-open") === "1",
  );
  // One right-hand panel at a time keeps the transcript readable.
  const [previewOpen, setPreviewOpen] = useState(() => !workspaceOpen);
  const [documents, setDocuments] = useState<DocumentSharingState>({
    grants: [],
  });
  const [pullRequests, setPullRequests] = useState<PullRequestState>({
    local: [],
  });
  const [documentReviews, setDocumentReviews] = useState<DocumentReviewState>({
    local: [],
  });
  const documentsRef = useRef<DocumentSharingHandle>(null);
  const repositoriesRef = useRef<RepositorySharingHandle>(null);
  const pullRequestsRef = useRef<PullRequestReviewHandle>(null);
  const documentReviewsRef = useRef<DocumentReviewHandle>(null);
  useEffect(() => {
    if (!signedIn()) return;
    const controller = new AbortController();
    void subscribe(controller.signal, setState, setLive);
    return () => controller.abort();
  }, []);
  useEffect(() => {
    if (!signedIn()) return;
    let cancelled = false;
    const refresh = async () => {
      try {
        const result = await api<Environment[]>("environments");
        if (!cancelled) setWorkspaces(result);
      } catch {
        /* Retried on the next tick. */
      }
    };
    void refresh();
    const timer = setInterval(refresh, 5000);
    window.addEventListener("warden-refresh-state", refresh);
    return () => {
      cancelled = true;
      clearInterval(timer);
      window.removeEventListener("warden-refresh-state", refresh);
    };
  }, []);
  useEffect(() => {
    sessionStorage.setItem("warden-workspace-open", workspaceOpen ? "1" : "0");
  }, [workspaceOpen]);
  // ⌘K / Ctrl+K opens the search palette from anywhere; again closes it.
  useEffect(() => {
    if (!signedIn()) return;
    const onKey = (event: KeyboardEvent) => {
      if (
        (event.metaKey || event.ctrlKey) &&
        !event.altKey &&
        !event.shiftKey &&
        event.key.toLowerCase() === "k"
      ) {
        event.preventDefault();
        setSearching((open) => !open);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);
  const chats = state.chats.filter((c) => c.archived === archived);
  const chat = chats.find((c) => c.id === selected) || chats[0];
  // A find request is for one chat; once the reader has moved on it is
  // forgotten, so coming back later does not replay the jump.
  useEffect(() => {
    if (find && chat?.id !== find.chatID) setFind(undefined);
  }, [chat?.id, find]);
  useEffect(() => {
    if (chat) {
      sessionStorage.setItem("warden-selected-chat", chat.id);
      history.replaceState(null, "", "?chat=" + encodeURIComponent(chat.id));
    }
  }, [chat?.id]);
  async function create(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const limits = state.sandboxes;
      const resources =
        !shared && size && limits && !sameSize(size, limits.default)
          ? size
          : undefined;
      const result = await api<{ id: string }>("chats", {
        title,
        sandboxID: shared,
        repository,
        provider,
        model,
        ...(resources ? { resources } : {}),
      });
      setSelected(result.id);
      setArchived(false);
      setCreating(false);
      setTitle("");
      setShared("");
      setRepository("");
      setSize(null);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  async function action(path: string, body?: unknown) {
    setError("");
    try {
      return await api(path, body);
    } catch (e) {
      setError(String(e));
    }
  }
  const refresh = () => window.dispatchEvent(new Event("warden-refresh-state"));
  useEffect(() => {
    if (!chat) return;
    let cancelled = false;
    let pending = false;
    setWorkspaceState("");
    const poll = async () => {
      if (pending) return;
      pending = true;
      try {
        const result = await api<{ sandbox?: { state: string } }>(
          `chats/${chat.id}/runtime`,
        );
        if (!cancelled) setWorkspaceState(result.sandbox?.state || "");
      } catch {
        /* A new chat may not have a workspace yet. */
      } finally {
        pending = false;
      }
    };
    void poll();
    const timer = setInterval(poll, 5000);
    window.addEventListener("warden-refresh-state", poll);
    return () => {
      cancelled = true;
      clearInterval(timer);
      window.removeEventListener("warden-refresh-state", poll);
    };
  }, [chat?.id, chat?.status]);
  useEffect(() => {
    let cancelled = false;
    const poll = async () => {
      try {
        const snapshot = await api<State>("state");
        if (!cancelled) setState(snapshot);
      } catch {
        /* The live stream handles reconnection. */
      }
    };
    window.addEventListener("warden-refresh-state", poll);
    return () => {
      cancelled = true;
      window.removeEventListener("warden-refresh-state", poll);
    };
  }, []);
  const onPreviewCount = useCallback((n: number) => setPreviewCount(n), []);
  const onDocuments = useCallback(
    (s: DocumentSharingState) => setDocuments(s),
    [],
  );
  const onPullRequests = useCallback(
    (s: PullRequestState) => setPullRequests(s),
    [],
  );
  const onDocumentReviews = useCallback(
    (s: DocumentReviewState) => setDocumentReviews(s),
    [],
  );
  if (!signedIn())
    return (
      <div className="signin">
        <Shield size={36} />
        <h1>Warden</h1>
        <p>Open Warden from its local launcher to access your chats.</p>
        <code>scripts/warden-chat open</code>
      </div>
    );
  const chatBusy =
    !!chat && ["running", "queued", "stopping"].includes(chat.status);
  const siblings = chat
    ? state.chats.filter(
        (c) => c.sandboxID === chat.sandboxID && c.id !== chat.id,
      )
    : [];
  const workspace = chat && workspaces.find((w) => w.id === chat.sandboxID);
  // Archived workspaces (every chat archived) and deleted ones sit with the
  // archived chats; the active list shows only live workspaces.
  const visibleWorkspaces = workspaces.filter(
    (w) => Boolean(w.deleted || w.archived) === archived,
  );
  const archiveWorkspace = (w: Environment) =>
    void action(`environments/${w.id}/archive`, {}).then(refresh);
  const showChat = (id: string) => {
    setAdminOpen(false);
    setSelected(id);
  };
  // From the palette: a chat, possibly archived, and the entry to land on.
  const openFound = (target: Chat, entryID?: string, query = "") => {
    setAdminOpen(false);
    setArchived(target.archived);
    setSelected(target.id);
    if (entryID) setFind({ chatID: target.id, query, entryID });
  };
  const findInChat = (query = "") => {
    if (chat) setFind({ chatID: chat.id, query });
  };
  // A workspace is shown through one of its chats. Prefer the chat already
  // open, then a live one; the oldest chat is often archived and would fall
  // outside the sidebar's current filter.
  const workspaceName = (w: Environment) =>
    w.chats.find((c) => !c.archived)?.title || w.name;
  const showWorkspace = (w: Environment) => {
    const target =
      w.chats.find((c) => c.id === chat?.id) ??
      w.chats.find((c) => !c.archived) ??
      w.chats[0];
    if (!target) return;
    setAdminOpen(false);
    setArchived(target.archived);
    setSelected(target.id);
    setPreviewOpen(false);
    setWorkspaceOpen(true);
  };
  // What the agent is asking for, answered from the transcript.
  const requests: RequestCard[] = [];
  if (chat && documents.pending) {
    const r = documents.pending;
    const what =
      r.access === "create"
        ? `to create a Google document “${r.title}”`
        : `for ${r.access === "write" ? "read and spreadsheet edit" : r.access === "structure" ? "read and full spreadsheet edit" : "read"} access to Google documents`;
    requests.push({
      id: "documents:" + r.request_id,
      icon: <FileText size={18} />,
      title: `${providerName(chat.provider)} asks ${what}`,
      detail: r.reason,
      note: siblings.length ? (
        <>
          Whatever you choose is shared with this workspace, including{" "}
          {siblings.map((c, i) => (
            <span key={c.id}>
              {i > 0 && ", "}
              <a
                href={"?chat=" + encodeURIComponent(c.id)}
                onClick={(e) => {
                  e.preventDefault();
                  showChat(c.id);
                }}
              >
                {c.title}
              </a>
            </span>
          ))}
          .
        </>
      ) : undefined,
      actions: [
        { label: "Decline", onClick: () => documentsRef.current?.decline() },
        {
          label:
            r.access === "create" ? "Review request…" : "Choose documents…",
          primary: true,
          onClick: () => documentsRef.current?.open(),
        },
      ],
    });
  }
  if (chat && pullRequests.pending) {
    const r = pullRequests.pending;
    requests.push({
      id: "pr:" + r.request_id,
      icon: <GitPullRequest size={18} />,
      title: `${providerName(chat.provider)} proposed a pull request: “${r.title}”`,
      detail: r.repository,
      actions: [
        {
          label: "Review proposal…",
          primary: true,
          onClick: () => pullRequestsRef.current?.open(r.request_id),
        },
      ],
    });
  }
  if (chat && documentReviews.pending) {
    const r = documentReviews.pending;
    requests.push({
      id: "doc:" + r.request_id,
      icon: <FileText size={18} />,
      title:
        r.status === "applying"
          ? `Writing suggested edits to “${r.title}”…`
          : `${providerName(chat.provider)} suggested ${plural(r.changes, "change")} to “${r.title}”`,
      detail: r.summary,
      actions: [
        {
          label: "Review suggestions…",
          primary: true,
          onClick: () => documentReviewsRef.current?.open(r.request_id),
        },
      ],
    });
  }
  const facts: string[] = [];
  if (chat) {
    const documentCount = workspace
      ? workspace.documents.reduce((n, g) => n + g.documents.length, 0)
      : documents.grants.reduce((n, g) => n + g.documents.length, 0);
    const repositoryCount =
      (workspace?.repositories.length ?? 0) + (chat.repository ? 1 : 0);
    const access = [
      documentCount ? plural(documentCount, "document") : "",
      repositoryCount
        ? plural(repositoryCount, "repository", "repositories")
        : "",
    ].filter(Boolean);
    if (access.length) facts.push(access.join(", "));
    if (siblings.length)
      facts.push(`shared with ${plural(siblings.length, "other chat")}`);
  }
  return (
    <div className="chat-workspace">
      <aside className="chat-sidebar">
        <div className="chat-brand">
          <Shield size={22} />
          <span>Warden</span>
        </div>
        <button
          className="chat-new-project"
          onClick={() => {
            setAdminOpen(false);
            setCreating(true);
          }}
        >
          <Plus size={16} />
          <span>New chat</span>
        </button>
        <button
          className="chat-search"
          aria-label="Search chats"
          title={`Search chats and messages (${modifierKey}K)`}
          onClick={() => setSearching(true)}
        >
          <Search size={16} />
          <span>Search</span>
          <kbd>{modifierKey}K</kbd>
        </button>
        <div className="chat-section-label">
          {archived ? "ARCHIVED" : "CHATS"}
        </div>
        <nav aria-label="Chats">
          {chats.map((c) => (
            <div className="chat-row" key={c.id}>
              <button
                className={c.id === chat?.id && !adminOpen ? "selected" : ""}
                onClick={() => showChat(c.id)}
              >
                <span
                  className={`status-dot ${c.startup && ["running", "queued"].includes(c.status) ? "starting" : c.status}`}
                  aria-label={
                    ["running", "queued"].includes(c.status)
                      ? chatStatusLabel(c)
                      : undefined
                  }
                  title={
                    ["running", "queued"].includes(c.status)
                      ? chatStatusLabel(c)
                      : undefined
                  }
                />
                <span>{c.title}</span>
              </button>
              <button
                className="chat-row-action"
                aria-label={`${c.archived ? "Restore" : "Archive"} chat ${c.title}`}
                title={c.archived ? "Restore chat" : "Archive chat"}
                disabled={
                  ["running", "queued", "stopping"].includes(c.status) ||
                  (c.archived &&
                    workspaces.some((w) => w.id === c.sandboxID && w.deleted))
                }
                onClick={() =>
                  void action(`chats/${c.id}/edit`, {
                    title: c.title,
                    archived: !c.archived,
                  }).then(refresh)
                }
              >
                {c.archived ? (
                  <ArchiveRestore size={14} />
                ) : (
                  <Archive size={14} />
                )}
              </button>
            </div>
          ))}
        </nav>
        {visibleWorkspaces.length > 0 && (
          <>
            <div className="chat-section-label">
              {archived ? "ARCHIVED WORKSPACES" : "WORKSPACES"}
            </div>
            <nav aria-label="Workspaces">
              {visibleWorkspaces.map((w) => (
                <div className="chat-row" key={w.id}>
                  <button
                    className={
                      workspaceOpen && !adminOpen && chat?.sandboxID === w.id
                        ? "selected"
                        : ""
                    }
                    onClick={() => showWorkspace(w)}
                  >
                    <span
                      className={`status-dot ${w.deleted ? "" : w.runtime?.state || ""}`}
                    />
                    <span>{workspaceName(w)}</span>
                    <small className="muted">
                      {plural(w.chats.length, "chat")}
                    </small>
                  </button>
                  {!w.deleted && !w.archived && (
                    <button
                      className="chat-row-action"
                      aria-label={`Archive workspace ${workspaceName(w)}`}
                      title="Archive workspace: stop it and archive its chats"
                      disabled={w.chats.some((c) =>
                        ["running", "queued", "stopping"].includes(c.status),
                      )}
                      onClick={() => archiveWorkspace(w)}
                    >
                      <Archive size={14} />
                    </button>
                  )}
                </div>
              ))}
            </nav>
          </>
        )}
        <div className="chat-sidebar-footer">
          {admin && (
            <button
              className={adminOpen ? "selected" : ""}
              aria-pressed={adminOpen}
              onClick={() => setAdminOpen(!adminOpen)}
            >
              <ShieldCheck size={16} />
              <span>Admin console</span>
            </button>
          )}
          <button
            title="Your standing instructions: the agent gets them in every chat you take part in"
            onClick={() => setInstructionsOpen(true)}
          >
            <NotebookPen size={16} />
            <span>Instructions</span>
          </button>
          <button onClick={() => setArchived(!archived)}>
            <Archive size={16} />
            <span>{archived ? "Active chats" : "Archived chats"}</span>
          </button>
          <div className={`chat-connection${live ? " live" : ""}`}>
            <span className={`status-dot${live ? " running" : ""}`} />
            <span>{live ? "Connected" : "Reconnecting"}</span>
          </div>
          {account}
        </div>
      </aside>
      {instructionsOpen && (
        <InstructionsDialog onClose={() => setInstructionsOpen(false)} />
      )}
      <main className="chat-main">
        {adminOpen && admin ? (
          <AdminConsole signIn={signIn} />
        ) : chat ? (
          <>
            <header className="chat-header">
              <div className="chat-title">
                <h1>{chat.title}</h1>
                <div className="chat-subtitle">
                  <span className={`status-dot ${workspaceState}`} />
                  <span>
                    {workspaceState
                      ? workspaceState.charAt(0).toUpperCase() +
                        workspaceState.slice(1)
                      : "Not started"}
                  </span>
                  <span>·</span>
                  <span>{providerName(chat.provider)}</span>
                  {facts.map((f) => (
                    <span key={f}>· {f}</span>
                  ))}
                </div>
              </div>
              <button
                className="ghost icon"
                aria-label="Find in chat"
                title={`Find in chat (${modifierKey}F)`}
                onClick={() => findInChat()}
              >
                <TextSearch size={16} />
              </button>
              {previewCount > 0 && (
                <button
                  className="ghost"
                  aria-pressed={previewOpen}
                  onClick={() => {
                    // One right-hand panel at a time keeps the transcript readable.
                    if (!previewOpen) setWorkspaceOpen(false);
                    setPreviewOpen(!previewOpen);
                  }}
                >
                  <PanelRight size={16} />
                  Preview
                </button>
              )}
              <button
                className="ghost"
                aria-pressed={workspaceOpen}
                onClick={() => {
                  if (!workspaceOpen) setPreviewOpen(false);
                  setWorkspaceOpen(!workspaceOpen);
                }}
              >
                <Box size={16} />
                Workspace
              </button>
              <details
                className="chat-menu"
                open={menuOpen}
                onToggle={(e) => setMenuOpen(e.currentTarget.open)}
                onBlur={(e) => {
                  if (!e.currentTarget.contains(e.relatedTarget as Node))
                    setMenuOpen(false);
                }}
                onKeyDown={(e) => {
                  if (e.key === "Escape") setMenuOpen(false);
                }}
              >
                <summary aria-label="Chat actions" role="button">
                  <MoreHorizontal size={18} />
                </summary>
                <div className="chat-menu-list" role="menu">
                  <button
                    role="menuitem"
                    onClick={() => {
                      setMenuOpen(false);
                      const next = prompt("Chat name", chat.title);
                      if (next)
                        void action(`chats/${chat.id}/edit`, {
                          title: next,
                          archived: chat.archived,
                        });
                    }}
                  >
                    <Pencil size={15} />
                    Rename
                  </button>
                  <button
                    role="menuitem"
                    onClick={() => {
                      setMenuOpen(false);
                      setExporting(true);
                    }}
                  >
                    <Download size={15} />
                    Export…
                  </button>
                  <button
                    role="menuitem"
                    onClick={() => {
                      setMenuOpen(false);
                      setChangesOpen(true);
                    }}
                  >
                    <FileDiff size={15} />
                    Changes…
                  </button>
                  <button
                    role="menuitem"
                    onClick={() => {
                      setMenuOpen(false);
                      setRewinding("");
                    }}
                  >
                    <History size={15} />
                    Rewind…
                  </button>
                  <button
                    role="menuitem"
                    disabled={chatBusy}
                    onClick={() => {
                      setMenuOpen(false);
                      void action(`chats/${chat.id}/edit`, {
                        title: chat.title,
                        archived: !chat.archived,
                      });
                    }}
                  >
                    {chat.archived ? (
                      <ArchiveRestore size={15} />
                    ) : (
                      <Archive size={15} />
                    )}
                    {chat.archived ? "Restore" : "Archive"}
                  </button>
                  <hr />
                  <button
                    role="menuitem"
                    onClick={() => {
                      setMenuOpen(false);
                      refresh();
                    }}
                  >
                    <RefreshCw size={15} />
                    Refresh workspace status
                  </button>
                  <button
                    role="menuitem"
                    onClick={() => {
                      setMenuOpen(false);
                      void action(`chats/${chat.id}/activity`, {});
                    }}
                  >
                    <Timer size={15} />
                    Keep workspace running
                  </button>
                </div>
              </details>
            </header>
            {error && (
              <p className="error" role="alert">
                {error}
              </p>
            )}
            {exporting && (
              <ExportDialog
                key={chat.id + "export"}
                chat={chat}
                onClose={() => setExporting(false)}
              />
            )}
            {rewinding !== null && (
              <RewindDialog
                key={chat.id + "rewind"}
                chat={chat}
                initial={rewinding || undefined}
                onClose={() => setRewinding(null)}
              />
            )}
            {changesOpen && (
              <SessionDiff
                key={chat.id + "changes"}
                chatID={chat.id}
                onClose={() => setChangesOpen(false)}
              />
            )}
            <div className="warden-chat-content">
              <Conversation
                key={chat.id}
                chat={chat}
                live={live}
                requests={requests}
                find={find}
                onExport={() => setExporting(true)}
                onRewind={(entryID) => setRewinding(entryID || "")}
                onChanges={() => setChangesOpen(true)}
                onModel={(next) =>
                  api(`chats/${chat.id}/agent`, {
                    provider: chat.provider || "codex",
                    model: next,
                  })
                }
                onMode={(mode) => api(`chats/${chat.id}/mode`, { mode })}
                onSettings={(change) =>
                  api(`chats/${chat.id}/settings`, change)
                }
                agentOptions={state.agentOptions}
              />
              <Previews
                key={chat.id + "preview"}
                chatID={chat.id}
                open={previewOpen}
                onCount={onPreviewCount}
              />
              {workspaceOpen && (
                <WorkspacePanel
                  key={chat.sandboxID}
                  chat={chat}
                  workspace={workspace}
                  siblings={siblings}
                  pullRequests={pullRequests.local}
                  documentReviews={documentReviews.local}
                  onSelectChat={showChat}
                  onShareDocuments={() => documentsRef.current?.open()}
                  onShareRepositories={() => repositoriesRef.current?.open()}
                  onOpenPullRequest={(id) => pullRequestsRef.current?.open(id)}
                  onOpenDocumentReview={(id) =>
                    documentReviewsRef.current?.open(id)
                  }
                  onChanged={refresh}
                  onChanges={() => setChangesOpen(true)}
                  limits={state.sandboxes}
                />
              )}
            </div>
            <DocumentSharing
              ref={documentsRef}
              key={chat.id + "documents"}
              chatID={chat.id}
              sandboxID={chat.sandboxID}
              siblings={siblings.map((c) => c.title)}
              canConnectGoogle={canConnectGoogle}
              autoOpen={false}
              trigger={null}
              onState={onDocuments}
            />
            <RepositorySharing
              ref={repositoriesRef}
              key={chat.id + "repositories"}
              chatID={chat.id}
              trigger={null}
              admin={admin}
            />
            <PullRequestReview
              ref={pullRequestsRef}
              key={chat.id + "reviews"}
              chatID={chat.id}
              autoOpen={false}
              trigger={null}
              onState={onPullRequests}
            />
            <DocumentReview
              ref={documentReviewsRef}
              key={chat.id + "document-reviews"}
              chatID={chat.id}
              author={providerName(chat.provider)}
              autoOpen={false}
              trigger={null}
              onState={onDocumentReviews}
            />
          </>
        ) : (
          <div className="empty">
            <Shield size={34} />
            <h1>Your agents, under your control.</h1>
            <p>Create a chat to start working in a Warden-managed workspace.</p>
            <button className="primary" onClick={() => setCreating(true)}>
              <Plus size={16} />
              Create a chat
            </button>
          </div>
        )}
      </main>
      {searching && (
        <SearchPalette
          chats={state.chats}
          current={adminOpen ? undefined : chat}
          onOpen={openFound}
          onFind={findInChat}
          onClose={() => setSearching(false)}
        />
      )}
      {creating && (
        <div className="modal-backdrop">
          <section
            className="modal"
            role="dialog"
            aria-modal="true"
            aria-labelledby="new-title"
          >
            <form onSubmit={create}>
              <h2 id="new-title">New chat</h2>
              <label>
                Name
                <input
                  autoFocus
                  placeholder="What are we working on?"
                  value={title}
                  onChange={(e) => setTitle(e.target.value)}
                  maxLength={160}
                />
              </label>
              <label>
                Provider
                <select
                  value={provider}
                  onChange={(e) => {
                    setProvider(e.target.value);
                    setModel("");
                  }}
                >
                  <option value="codex">Codex</option>
                  <option value="claude">Claude</option>
                </select>
              </label>
              <label>
                Model
                <ModelSelect
                  provider={provider}
                  value={model}
                  onChange={setModel}
                  label="New conversation model"
                />
              </label>
              <label>
                Workspace
                <select
                  value={shared}
                  onChange={(e) => setShared(e.target.value)}
                >
                  <option value="">Create a fresh workspace</option>
                  {workspaces
                    .filter((w) => !w.deleted)
                    .map((w) => (
                      <option key={w.id} value={w.id}>
                        Share {workspaceName(w)}
                        {w.chats.length > 1
                          ? ` (${plural(w.chats.length, "chat")})`
                          : ""}
                      </option>
                    ))}
                </select>
              </label>
              {!shared && (
                <label>
                  <span>
                    Public GitHub repository{" "}
                    <span className="muted">(optional)</span>
                  </span>
                  <input
                    placeholder="github://owner/repository"
                    value={repository}
                    onChange={(e) => setRepository(e.target.value)}
                  />
                </label>
              )}
              {!shared && state.sandboxes && (
                <fieldset className="size-fieldset">
                  <legend>Size</legend>
                  <SizeSelect
                    limits={state.sandboxes}
                    value={size ?? state.sandboxes.default}
                    onChange={setSize}
                  />
                  <p className="muted">
                    {size && !sameSize(size, state.sandboxes.default)
                      ? state.sandboxes.restart
                        ? "A workspace of a non-default size is created from scratch, so the first message takes a little longer than usual."
                        : "The agent can ask for more later; you can change it any time from the workspace panel."
                      : "The default. The agent can ask for more later; you can change it any time from the workspace panel."}
                  </p>
                </fieldset>
              )}
              <p className="muted">
                {shared
                  ? "Chats keep separate histories, but the files, shared documents, repositories and previews belong to the workspace and are available to every chat in it."
                  : "Your agent gets a fresh sandbox when you send the first message."}
              </p>
              {error && <p role="alert">{error}</p>}
              <div className="button-row">
                <button type="button" onClick={() => setCreating(false)}>
                  Cancel
                </button>
                <button className="primary" disabled={busy || !live}>
                  Create chat
                </button>
              </div>
            </form>
          </section>
        </div>
      )}
    </div>
  );
}
