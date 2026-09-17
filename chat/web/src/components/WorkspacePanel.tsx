import { useState } from "react";
import { Archive, ExternalLink, History, Square, Trash2 } from "lucide-react";
import type { AccessEvent, Chat, Environment, SandboxUsage } from "../types";
import { api } from "../api";
import type { PullRequestProposal } from "./PullRequestReview";

const remaining = (value: number | null) => {
  if (!value) return "";
  const minutes = Math.max(0, Math.round((value - Date.now() / 1000) / 60));
  if (minutes < 60) return `${minutes} min`;
  if (minutes < 24 * 60) return `${Math.round(minutes / 60)} h`;
  return `${Math.round(minutes / 1440)} d`;
};
const when = (value?: number | null) =>
  value ? new Date(value * 1000).toLocaleString() : "";
// One line per access event, for the history list.
function describe(e: AccessEvent): string {
  if (e.kind === "repositories_selected") {
    const names = Object.entries(e.repositories || {});
    if (!names.length) return "Repository sharing cleared";
    return (
      "Repositories shared: " +
      names
        .map(
          ([n, a]) =>
            `${n} (${(a || "").replaceAll("pull_requests", "pull requests").replaceAll("contents", "code").replaceAll(",", ", ")})`,
        )
        .join(", ")
    );
  }
  if (e.kind === "github_disconnected") return "GitHub disconnected";
  const docs = (e.documents || []).map((d) => d.title).join(", ");
  const what = docs || e.title || "documents";
  const access =
    e.access === "read"
      ? "read"
      : e.access === "create"
        ? "create"
        : e.access === "structure"
          ? "full edit"
          : "edit";
  switch (e.status) {
    case "pending":
      return `Requested ${access} access to ${what}`;
    case "granted":
      return `${e.expired ? "Expired" : "Granted"} ${access} access to ${what}${e.expires_at ? ` until ${when(e.expires_at)}` : ""}`;
    case "denied":
      return `Denied ${access} access to ${what}`;
    case "revoked":
      return `Revoked ${access} access to ${what}`;
    case "failed":
      return `Failed to create ${what}`;
    default:
      return `${e.status} ${what}`;
  }
}
const gib = (bytes: number) => `${(bytes / 1024 ** 3).toFixed(1)} GiB`;
const percent = (used: number, total: number) =>
  total > 0 ? Math.max(0, Math.min(100, (100 * used) / total)) : 0;

/* One row per resource: what is used of what was provisioned, with a bar.
   A stopped sandbox shows only the provisioned side; the disk size of a
   stopped sandbox is unknown until it boots. */
function Resources({ usage }: { usage: SandboxUsage }) {
  const rows: { name: string; value: string; percent: number | null }[] = [
    {
      name: "CPU",
      value: !usage.running
        ? `${usage.cpus} provisioned`
        : usage.cpuPercent == null
          ? `sampling… · ${usage.cpus} provisioned`
          : `${usage.cpuPercent.toFixed(0)}% of ${usage.cpus}`,
      percent: usage.running ? usage.cpuPercent : null,
    },
    {
      name: "Memory",
      value: usage.running
        ? `${gib(usage.memoryUsed)} of ${gib(usage.memoryTotal)}`
        : `${gib(usage.memoryTotal)} provisioned`,
      percent: usage.running
        ? percent(usage.memoryUsed, usage.memoryTotal)
        : null,
    },
    {
      name: "Disk",
      value: usage.running
        ? `${gib(usage.diskUsed)} of ${gib(usage.diskTotal)}`
        : usage.diskTotal > 0
          ? `${gib(usage.diskTotal)} provisioned`
          : "sized at boot",
      percent: usage.running ? percent(usage.diskUsed, usage.diskTotal) : null,
    },
  ];
  return (
    <section className="workspace-section">
      <h2>Resources</h2>
      <ul className="workspace-resources">
        {rows.map((r) => (
          <li key={r.name}>
            <span>{r.name}</span>
            <small>{r.value}</small>
            <meter
              min={0}
              max={100}
              value={r.percent ?? 0}
              aria-label={`${r.name} used`}
              className={r.percent == null ? "idle" : ""}
            />
          </li>
        ))}
      </ul>
    </section>
  );
}
const label = (state: string) =>
  state ? state.charAt(0).toUpperCase() + state.slice(1) : "Not started";

/* The workspace is the sandbox behind one or more chats. Everything here is
   reachable by every chat in it, which is why it is managed here, not per
   chat. Agent requests are answered in the transcript; this shows state. */
export function WorkspacePanel({
  chat,
  workspace,
  siblings,
  pullRequests,
  onSelectChat,
  onShareDocuments,
  onShareRepositories,
  onOpenPullRequest,
  onChanged,
}: {
  chat: Chat;
  workspace?: Environment;
  siblings: Chat[];
  pullRequests: PullRequestProposal[];
  onSelectChat: (id: string) => void;
  onShareDocuments: () => void;
  onShareRepositories: () => void;
  onOpenPullRequest: (id: string) => void;
  onChanged: () => void;
}) {
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [history, setHistory] = useState<AccessEvent[]>();
  const [historyError, setHistoryError] = useState("");
  const ws = workspace;
  async function loadHistory() {
    if (!ws) return;
    setHistoryError("");
    try {
      const r = await api<{ events: AccessEvent[] }>(
        `sharing/history?sandboxID=${encodeURIComponent(ws.id)}`,
      );
      setHistory(r.events);
    } catch (e) {
      setHistoryError(String(e));
    }
  }
  const chats = ws?.chats ?? [
    { id: chat.id, title: chat.title, status: chat.status, archived: false },
    ...siblings.map((c) => ({
      id: c.id,
      title: c.title,
      status: c.status,
      archived: c.archived,
    })),
  ];
  const running = chats.some((c) =>
    ["running", "queued", "stopping"].includes(c.status),
  );
  const state = ws?.deleted ? "deleted" : ws?.runtime?.state || "";
  const documents = ws?.documents ?? [];
  const repositories = ws?.repositories ?? [];
  const ports = ws?.ports ?? [];
  async function act(what: string, path: string, body?: unknown) {
    setBusy(what);
    setError("");
    try {
      await api(path, body ?? {});
      onChanged();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy("");
    }
  }
  function remove() {
    const parts = [`${chats.length} chat${chats.length === 1 ? "" : "s"}`];
    if (documents.length)
      parts.push(
        `${documents.length} document grant${documents.length === 1 ? "" : "s"}`,
      );
    if (repositories.length)
      parts.push(
        `${repositories.length} shared repositor${repositories.length === 1 ? "y" : "ies"}`,
      );
    if (ports.length)
      parts.push(`${ports.length} preview${ports.length === 1 ? "" : "s"}`);
    if (
      confirm(
        `Delete this workspace?\n\nThis stops its sandbox, deletes its files, archives its ${parts.join(", revokes its ")} and cannot be undone.`,
      )
    )
      void act("delete", `environments/${chat.sandboxID}/delete`);
  }
  return (
    <aside className="workspace-panel" aria-label="Workspace">
      <header className="workspace-head">
        <div className="workspace-status">
          <span className={`status-dot ${state}`} />
          <span>{label(state)}</span>
          {ws?.runtime && (
            <span className="muted">· {ws.runtime.runtimeName}</span>
          )}
        </div>
        {!ws?.deleted && (
          <div className="workspace-actions">
            <button
              disabled={!!busy || running || !state || state === "stopped"}
              onClick={() =>
                void act("stop", `environments/${chat.sandboxID}/stop`)
              }
            >
              <Square size={12} fill="currentColor" />
              {busy === "stop" ? "Stopping…" : "Stop"}
            </button>
            {!ws?.archived && (
              <button
                disabled={!!busy || running}
                title="Stop the workspace and archive its chats; files are kept"
                onClick={() =>
                  void act("archive", `environments/${chat.sandboxID}/archive`)
                }
              >
                <Archive size={14} />
                {busy === "archive" ? "Archiving…" : "Archive"}
              </button>
            )}
            <button
              className="danger"
              disabled={!!busy || running}
              onClick={remove}
            >
              <Trash2 size={14} />
              {busy === "delete" ? "Deleting…" : "Delete"}
            </button>
          </div>
        )}
      </header>
      {error && (
        <p role="alert" className="error">
          {error}
        </p>
      )}
      {running && (
        <p className="workspace-note">
          A chat is running here. Stop it before stopping or deleting the
          workspace.
        </p>
      )}
      {ws?.deleted && (
        <p className="workspace-note">
          This workspace was deleted. Its chats are archived and cannot be
          resumed.
        </p>
      )}
      {ws?.usage && !ws.deleted && <Resources usage={ws.usage} />}
      <section className="workspace-section">
        <h2>Chats in this workspace</h2>
        <ul>
          {chats.map((c) => (
            <li key={c.id}>
              <span className={`status-dot ${c.status}`} />
              {c.id === chat.id ? (
                <span className="workspace-current">{c.title}</span>
              ) : (
                <a
                  href={"?chat=" + encodeURIComponent(c.id)}
                  onClick={(e) => {
                    e.preventDefault();
                    onSelectChat(c.id);
                  }}
                >
                  {c.title}
                </a>
              )}
              <small>
                {c.id === chat.id
                  ? "this chat"
                  : c.archived
                    ? "archived"
                    : c.status}
              </small>
            </li>
          ))}
        </ul>
      </section>
      <section className="workspace-section">
        <h2>
          Documents
          {!ws?.deleted && (
            <button className="ghost" onClick={onShareDocuments}>
              Share documents…
            </button>
          )}
        </h2>
        {!documents.length && <p className="muted">None shared.</p>}
        <ul>
          {documents.flatMap((g) =>
            g.documents.map((d) => (
              <li
                key={g.request_id + d.id}
                className={g.expired ? "expired" : ""}
              >
                <a href={d.url} target="_blank" rel="noreferrer">
                  {d.title}
                </a>
                <small>
                  {g.expired
                    ? "access expired"
                    : `${g.access === "read" ? "read" : g.access === "write" ? "edit" : "full edit"} · ${remaining(g.expires_at)}`}
                </small>
              </li>
            )),
          )}
        </ul>
      </section>
      <section className="workspace-section">
        <h2>
          Repositories
          {!ws?.deleted && (
            <button className="ghost" onClick={onShareRepositories}>
              Share repositories…
            </button>
          )}
        </h2>
        {!chat.repository && !repositories.length && (
          <p className="muted">None.</p>
        )}
        <ul>
          {chat.repository && (
            <li>
              <code>{chat.repository.replace(/^github:\/\//, "")}</code>
              <small>checked out here</small>
            </li>
          )}
          {repositories.map((r) => (
            <li key={r.id}>
              <a href={r.url} target="_blank" rel="noreferrer">
                <code>{r.full_name}</code>
              </a>
              <small>{r.access_summary || "read access"}</small>
            </li>
          ))}
        </ul>
      </section>
      {ws && (
        <section className="workspace-section">
          <details
            className="workspace-history"
            onToggle={(e) => {
              if (e.currentTarget.open && history === undefined)
                void loadHistory();
            }}
          >
            <summary>
              <History size={13} />
              Access history
            </summary>
            {historyError && (
              <p className="error" role="alert">
                {historyError}
              </p>
            )}
            {history && !history.length && (
              <p className="muted">
                Nothing has been shared with this workspace.
              </p>
            )}
            {history && history.length > 0 && (
              <ul className="workspace-history-list">
                {history.map((e, i) => (
                  <li
                    key={i}
                    className={
                      e.status === "granted" && !e.expired ? "active" : ""
                    }
                  >
                    <span>{describe(e)}</span>
                    <small>
                      {when(e.resolved_at || e.created_at)}
                      {e.resolved_by ? ` · ${e.resolved_by}` : ""}
                    </small>
                  </li>
                ))}
              </ul>
            )}
            {history && (
              <button className="ghost" onClick={() => void loadHistory()}>
                Refresh
              </button>
            )}
          </details>
        </section>
      )}
      {pullRequests.length > 0 && (
        <section className="workspace-section">
          <h2>Pull requests</h2>
          <ul>
            {pullRequests.map((r) => (
              <li key={r.request_id}>
                <a
                  href="#"
                  onClick={(e) => {
                    e.preventDefault();
                    onOpenPullRequest(r.request_id);
                  }}
                >
                  {r.title}
                </a>
                <small>{r.status}</small>
              </li>
            ))}
          </ul>
        </section>
      )}
      <section className="workspace-section">
        <h2>Previews</h2>
        {!ports.length && <p className="muted">None published.</p>}
        <ul>
          {ports.map((p) => (
            <li key={p.id}>
              <a href={p.url} target="_blank" rel="noopener noreferrer">
                {p.title} · :{p.port} <ExternalLink size={12} />
              </a>
              <button
                className="ghost"
                disabled={!!busy}
                onClick={() => void act("unpublish", `ports/${p.id}/revoke`)}
              >
                Unpublish
              </button>
            </li>
          ))}
        </ul>
      </section>
    </aside>
  );
}
