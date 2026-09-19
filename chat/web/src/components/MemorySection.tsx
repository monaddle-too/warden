import { useState } from "react";
import { BookOpen, FilePlus2 } from "lucide-react";
import { api } from "../api";
import { formatSize } from "../attachments";
import {
  groupMemory,
  memoryDescription,
  memoryLabel,
  memoryPathError,
  missingWorkspaceFiles,
  rulePath,
} from "../memory";
import type { Chat, MemoryFile, MemoryView } from "../types";
import { MemoryDialog } from "./MemoryDialog";

type Editing = Pick<MemoryFile, "scope" | "path" | "text" | "truncated"> & {
  size?: number;
};

/* The workspace panel's Memory section (parity item 13): the workspace's
   CLAUDE.md and its kin, the rules under .claude/rules and the agent CLI's
   auto-memory files, listed from the sandbox when the section is opened
   (one guest round-trip; the sandbox must be running), each opening the
   editor. The hint says whether the agent's launch reads them at all. */
export function MemorySection({
  chat,
  disabled = false,
}: {
  chat: Chat;
  disabled?: boolean;
}) {
  const [view, setView] = useState<MemoryView>();
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [editing, setEditing] = useState<Editing>();
  async function load() {
    setLoading(true);
    setError("");
    try {
      setView(
        await api<MemoryView>(`chats/${encodeURIComponent(chat.id)}/memory`),
      );
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }
  function create(path: string, scope: MemoryFile["scope"] = "workspace") {
    const problem = memoryPathError(scope, path);
    if (problem) {
      setError(problem);
      return;
    }
    setEditing({ scope, path, text: "" });
  }
  function newRule() {
    const name = prompt(
      "Name of the rule file under .claude/rules (for example style, or go/errors):",
    );
    if (name === null) return;
    const path = rulePath(name);
    if (!path) return;
    create(path);
  }
  const groups = view ? groupMemory(view.files) : [];
  const missing = view ? missingWorkspaceFiles(view.files) : [];
  return (
    <section className="workspace-section workspace-memory">
      <details
        onToggle={(e) => {
          if (e.currentTarget.open && view === undefined && !loading)
            void load();
        }}
      >
        <summary>
          <BookOpen size={13} />
          Memory
        </summary>
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
        {loading && view === undefined && <p className="muted">Reading…</p>}
        {view && (
          <>
            {view.hint && <p className="muted workspace-note">{view.hint}</p>}
            {groups.map((g) => (
              <div key={g.kind} className="workspace-memory-group">
                <h3>{g.title}</h3>
                <ul>
                  {g.files.map((f) => (
                    <li key={f.scope + ":" + f.path}>
                      <button
                        className="link"
                        title={
                          f.scope === "auto"
                            ? `${view.autoDir}/${f.path}`
                            : `${view.root}/${f.path}`
                        }
                        disabled={disabled}
                        onClick={() => setEditing(f)}
                      >
                        <code>{memoryLabel(f)}</code>
                      </button>
                      <small>
                        {formatSize(f.size)}
                        {f.truncated ? " · cut" : ""}
                        {g.kind === "rules" || g.kind === "auto"
                          ? ""
                          : ` · ${memoryDescription(f)}`}
                      </small>
                    </li>
                  ))}
                </ul>
              </div>
            ))}
            {!view.files.length && (
              <p className="muted">
                No memory files yet: no CLAUDE.md, rules or auto-memory in this
                workspace.
              </p>
            )}
            {!disabled && (
              <div className="workspace-memory-actions">
                {missing.includes("CLAUDE.md") && (
                  <button className="ghost" onClick={() => create("CLAUDE.md")}>
                    <FilePlus2 size={13} />
                    Create CLAUDE.md
                  </button>
                )}
                {missing.includes("AGENTS.md") && chat.provider === "codex" && (
                  <button className="ghost" onClick={() => create("AGENTS.md")}>
                    <FilePlus2 size={13} />
                    Create AGENTS.md
                  </button>
                )}
                <button className="ghost" onClick={newRule}>
                  <FilePlus2 size={13} />
                  New rule…
                </button>
                <button
                  className="ghost"
                  disabled={loading}
                  onClick={() => void load()}
                >
                  {loading ? "Reading…" : "Refresh"}
                </button>
              </div>
            )}
          </>
        )}
      </details>
      {editing && (
        <MemoryDialog
          key={editing.scope + ":" + editing.path}
          chatID={chat.id}
          file={editing}
          onSaved={() => void load()}
          onClose={() => setEditing(undefined)}
        />
      )}
    </section>
  );
}
