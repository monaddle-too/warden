import { useEffect, useState } from "react";
import {
  Archive,
  ExternalLink,
  History,
  Play,
  Square,
  Trash2,
} from "lucide-react";
import type {
  AccessEvent,
  Chat,
  Environment,
  PodInfo,
  ResourceLimits,
  Resources,
  SandboxUsage,
} from "../types";
import { stageLabel } from "../stages";
import { cpu, memory } from "../units";
import { api } from "../api";
import type { PullRequestProposal } from "./PullRequestReview";
import { resourcesLabel } from "./Approvals";
import { SizeSelect, sameSize } from "./SizeSelect";
import { NetworkSelect } from "./NetworkSelect";
import {
  effectiveNetwork,
  networkTitle,
  type InstallNetwork,
  type NetworkMode,
} from "../network";
import type { DocumentProposal } from "./DocumentReview";
import { MemorySection } from "./MemorySection";
import { PermissionsSection } from "./PermissionsSection";
import { chatSpend, spendLine, spendSummary, workspaceSpend } from "../spend";

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
          ? "full spreadsheet edit"
          : "spreadsheet edit";
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
   stopped sandbox is unknown until it boots. The section around it holds
   the owner's size change. */
function UsageRows({ usage }: { usage: SandboxUsage }) {
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
  );
}
/* The sandbox pod on Kubernetes: where it runs and what it was given. */
function Pod({ pod }: { pod: PodInfo }) {
  const facts: [string, string][] = [
    ["Pod", `${pod.namespace}/${pod.name}`],
    ["Node", pod.node || "not scheduled yet"],
    [
      "State",
      pod.ready
        ? "Running"
        : pod.reason
          ? `${pod.phase} · ${pod.reason}`
          : pod.phase,
    ],
  ];
  if (pod.runtimeClass) facts.push(["Isolation", pod.runtimeClass]);
  if (pod.ip) facts.push(["Address", pod.ip]);
  if (pod.started)
    facts.push(["Started", new Date(pod.started).toLocaleString()]);
  if (pod.restarts) facts.push(["Restarts", String(pod.restarts)]);
  facts.push([
    "CPU",
    pod.usage
      ? `${cpu(pod.usage.cpuMilli)} used of ${cpu(pod.limits.cpuMilli || pod.requests.cpuMilli)}`
      : `${cpu(pod.limits.cpuMilli || pod.requests.cpuMilli)} limit`,
  ]);
  facts.push([
    "Memory",
    pod.usage
      ? `${memory(pod.usage.memoryBytes)} used of ${memory(pod.limits.memoryBytes || pod.requests.memoryBytes)}`
      : `${memory(pod.limits.memoryBytes || pod.requests.memoryBytes)} limit`,
  ]);
  return (
    <section className="workspace-section">
      <h2>Pod</h2>
      <dl className="workspace-facts">
        {facts.map(([k, v]) => (
          <div key={k}>
            <dt>{k}</dt>
            <dd title={v}>{v}</dd>
          </div>
        ))}
      </dl>
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
  documentReviews = [],
  onSelectChat,
  onShareDocuments,
  onShareRepositories,
  onOpenPullRequest,
  onOpenDocumentReview,
  onChanged,
  onChanges,
  limits,
  owner = false,
}: {
  chat: Chat;
  workspace?: Environment;
  siblings: Chat[];
  pullRequests: PullRequestProposal[];
  // The runner's size offer; absent while the runner is unreachable.
  limits?: ResourceLimits;
  // The owner may change the workspace's network access.
  owner?: boolean;
  documentReviews?: DocumentProposal[];
  onSelectChat: (id: string) => void;
  onShareDocuments: () => void;
  onShareRepositories: () => void;
  onOpenPullRequest: (id: string) => void;
  onOpenDocumentReview?: (id: string) => void;
  onChanged: () => void;
  /* Opens the session diff: the workspace's changes since the chat began
     (rewind.ts). */
  onChanges?: () => void;
}) {
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [history, setHistory] = useState<AccessEvent[]>();
  const [historyError, setHistoryError] = useState("");
  // The size being edited, or null when the row shows the current size.
  const [sizing, setSizing] = useState<Resources | null>(null);
  // The network access being edited (network.ts), or null when the row
  // shows the current one; the install's setting is what "" means.
  const [networking, setNetworking] = useState<NetworkMode | null>(null);
  const [installNetwork, setInstallNetwork] = useState<InstallNetwork>();
  useEffect(() => {
    api<InstallNetwork>("sharing/egress").then(setInstallNetwork, () => {});
  }, [chat.sandboxID]);
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
  // The workspace's spend: this chat and its siblings (spend.ts).
  const total = workspaceSpend([chat, ...siblings], chat.sandboxID);
  const chats = ws?.chats ?? [
    {
      id: chat.id,
      title: chat.title,
      status: chat.status,
      archived: false,
      stage: chat.startup?.stage,
    },
    ...siblings.map((c) => ({
      id: c.id,
      title: c.title,
      status: c.status,
      archived: c.archived,
      stage: c.startup?.stage,
    })),
  ];
  const running = chats.some((c) =>
    ["running", "queued", "stopping"].includes(c.status),
  );
  const state = ws?.deleted ? "deleted" : ws?.runtime?.state || "";
  // The workspace's own network access; "" follows the install.
  const network: NetworkMode = ws?.network ?? chat.network ?? "";
  // The size in force: the runner's record, else what the first chat asked
  // for, else the runner's default.
  const current: Resources = ws?.resources ??
    chat.resources ??
    limits?.default ?? { cpuMilli: 1000, memoryMB: 1536 };
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
            {!running &&
            (!state || state === "stopped" || state === "error") ? (
              <button
                disabled={!!busy || !ws?.runtime}
                title={
                  ws?.runtime
                    ? "Start the workspace's sandbox now, so the next message starts at once"
                    : "The first message creates the sandbox"
                }
                onClick={() =>
                  void act("start", `environments/${chat.sandboxID}/start`)
                }
              >
                <Play size={12} fill="currentColor" />
                {busy === "start" ? "Starting…" : "Start"}
              </button>
            ) : (
              <button
                disabled={!!busy || state === "stopping"}
                title={
                  running
                    ? "Stop the running chat and the workspace's sandbox; files are kept"
                    : "Stop the workspace's sandbox; files are kept"
                }
                onClick={() =>
                  void act("stop", `environments/${chat.sandboxID}/stop`)
                }
              >
                <Square size={12} fill="currentColor" />
                {busy === "stop" ? "Stopping…" : "Stop"}
              </button>
            )}
            {!ws?.archived && (
              <button
                disabled={!!busy}
                title="Stop the workspace (and any running chat) and archive its chats; files are kept"
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
              title={
                running
                  ? "Stop the running chat first; deleting removes the workspace's files"
                  : "Delete the workspace and its files"
              }
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
          A chat is running here. Stop stops it too; delete needs it stopped
          first.
        </p>
      )}
      {ws?.deleted && (
        <p className="workspace-note">
          This workspace was deleted. Its chats are archived and cannot be
          resumed.
        </p>
      )}
      {ws?.copiedFrom && (
        <p className="workspace-note workspace-origin">
          Copied from{" "}
          <a
            href={"?chat=" + encodeURIComponent(ws.copiedFrom.chatID)}
            title="Open the chat this workspace was forked from"
            onClick={(e) => {
              e.preventDefault();
              onSelectChat(ws.copiedFrom!.chatID);
            }}
          >
            {ws.copiedFrom.name || "another workspace"}
          </a>{" "}
          at {when(ws.copiedFrom.at)}. Its files came along; shared documents,
          repositories and network stay with the original until shared here.
        </p>
      )}
      {!ws?.deleted && (limits || ws?.usage) && (
        <section className="workspace-section">
          <h2>
            Resources
            {limits && !sizing && (
              <button
                className="ghost"
                disabled={!!busy || (!!ws?.resizing && !ws.resizing.done)}
                title="Change the CPUs and memory this workspace gets"
                onClick={() => setSizing(current)}
              >
                Change…
              </button>
            )}
          </h2>
          {ws?.resizing && !ws.resizing.done && (
            <p className="muted">
              Resizing to{" "}
              {resourcesLabel(
                ws.resizing.target.cpuMilli,
                ws.resizing.target.memoryMB,
              )}
              … the sandbox is being replaced; a chat resumes on its next
              message.
            </p>
          )}
          {ws?.resizing?.error && (
            <p role="alert" className="error">
              Resize to{" "}
              {resourcesLabel(
                ws.resizing.target.cpuMilli,
                ws.resizing.target.memoryMB,
              )}{" "}
              failed: {ws.resizing.error}
            </p>
          )}
          {!sizing && ws?.usage && <UsageRows usage={ws.usage} />}
          {!sizing && !ws?.usage && (
            <p>
              {resourcesLabel(current.cpuMilli, current.memoryMB)}
              {limits && sameSize(current, limits.default) && (
                <span className="muted"> · default</span>
              )}
            </p>
          )}
          {sizing && limits && (
            <form
              className="size-form"
              onSubmit={(e) => {
                e.preventDefault();
                void act("resize", `environments/${chat.sandboxID}/resize`, {
                  resources: sizing,
                }).then(() => setSizing(null));
              }}
            >
              <SizeSelect
                limits={limits}
                value={sizing}
                onChange={setSizing}
                disabled={busy === "resize"}
              />
              <p className="muted">
                {limits.restart
                  ? "The sandbox is recreated at the new size; files are kept."
                  : "Applied in place where the cluster allows it, otherwise the sandbox restarts at the new size; files are kept."}{" "}
                {running &&
                  (limits.restart
                    ? "The running chat is stopped first. "
                    : "A restart stops the running chat first. ")}
                Up to {resourcesLabel(limits.max.cpuMilli, limits.max.memoryMB)}
                .
              </p>
              <div className="button-row">
                <button
                  type="button"
                  onClick={() => setSizing(null)}
                  disabled={busy === "resize"}
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  className="primary"
                  disabled={busy === "resize" || sameSize(sizing, current)}
                >
                  {busy === "resize" ? "Resizing…" : "Resize"}
                </button>
              </div>
            </form>
          )}
        </section>
      )}
      {!ws?.deleted && (
        <section className="workspace-section">
          <h2>
            Network access
            {owner && networking === null && (
              <button
                className="ghost"
                disabled={!!busy}
                title="Choose what this workspace's sandbox may reach"
                onClick={() => setNetworking(network)}
              >
                Change…
              </button>
            )}
          </h2>
          {networking === null && (
            <p>
              {effectiveNetwork(network, installNetwork)
                ? networkTitle(effectiveNetwork(network, installNetwork)!)
                : "Install setting"}
              {!network && (
                <span className="muted"> · the install's setting</span>
              )}
            </p>
          )}
          {networking !== null && (
            <form
              className="size-form"
              onSubmit={(e) => {
                e.preventDefault();
                void act("network", `environments/${chat.sandboxID}/network`, {
                  network: networking,
                }).then(() => setNetworking(null));
              }}
            >
              <NetworkSelect
                value={networking}
                install={installNetwork}
                onChange={setNetworking}
                disabled={busy === "network"}
              />
              <p className="muted">
                Applies to every chat in this workspace at once, running ones
                included. Credentials are still injected only for approved
                requests.
              </p>
              <div className="button-row">
                <button
                  type="button"
                  onClick={() => setNetworking(null)}
                  disabled={busy === "network"}
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  className="primary"
                  disabled={busy === "network" || networking === network}
                >
                  {busy === "network" ? "Applying…" : "Apply"}
                </button>
              </div>
            </form>
          )}
        </section>
      )}
      {ws?.pod && !ws.deleted && <Pod pod={ws.pod} />}
      <section className="workspace-section">
        <h2>Chats in this workspace</h2>
        <ul>
          {chats.map((c) => (
            <li key={c.id}>
              <span
                className={`status-dot ${c.stage ? "starting" : c.status}`}
              />
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
                    : c.stage
                      ? stageLabel(c.stage).toLowerCase()
                      : c.status}
              </small>
            </li>
          ))}
        </ul>
      </section>
      <section className="workspace-section">
        <h2>Spend</h2>
        <dl className="workspace-facts">
          <div>
            <dt>Workspace</dt>
            <dd
              title={`${spendLine(total.spend)} · ${total.chats} chat${total.chats === 1 ? "" : "s"}`}
            >
              {spendSummary(total.spend)}
              {total.chats > 1 ? ` · ${total.chats} chats` : ""}
            </dd>
          </div>
          {total.chats > 1 && (
            <div>
              <dt>This chat</dt>
              <dd title={spendLine(chatSpend(chat))}>
                {spendSummary(chatSpend(chat))}
              </dd>
            </div>
          )}
        </dl>
        <p className="muted">
          Every chat of the workspace summed from the agent's turns, archived
          ones included; Codex reports tokens and no cost.
        </p>
      </section>
      {onChanges && !ws?.deleted && (
        <section className="workspace-section">
          <h2>
            Changes
            <button
              className="ghost"
              title="What changed in the workspace since this chat began"
              onClick={onChanges}
            >
              View…
            </button>
          </h2>
          <p className="muted">
            The workspace against the checkpoint taken before this chat's first
            message, or its last code rewind.
          </p>
        </section>
      )}
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
                    : `${g.access === "write" ? "spreadsheet edit" : g.access === "structure" ? "full spreadsheet edit" : "read"} · ${remaining(g.expires_at)}`}
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
      <MemorySection chat={chat} disabled={!!ws?.deleted} />
      {chat.provider === "claude" && (
        <PermissionsSection
          chat={chat}
          workspace={ws}
          siblings={siblings}
          disabled={!!ws?.deleted}
          onChanged={onChanged}
        />
      )}
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
      {documentReviews.length > 0 && (
        <section className="workspace-section">
          <h2>Document suggestions</h2>
          <ul>
            {documentReviews.map((r) => (
              <li key={r.request_id}>
                <a
                  href="#"
                  onClick={(e) => {
                    e.preventDefault();
                    onOpenDocumentReview?.(r.request_id);
                  }}
                >
                  {r.title}: {r.summary}
                </a>
                <small>{r.status}</small>
              </li>
            ))}
          </ul>
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
