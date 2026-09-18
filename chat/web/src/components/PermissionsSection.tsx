import { useState, type FormEvent } from "react";
import { api } from "../api";
import { RULE_EXAMPLES, RULE_KINDS, kindHint, ruleError, ruleOrigin } from "../rules";
import type { Chat, Environment, Rule } from "../types";

/* The workspace panel's Permissions section (parity round 2 B): the
   workspace's permission rules — allow / deny / ask by a tool pattern in
   Claude Code's syntax — applied to every chat of it, with where each came
   from (the editor, or "Allow always" from a chat) and who added it; add
   and remove. Under them, each chat's own rules, read-only with remove.
   The rules are Warden's (chats/rules.go): they answer the CLI's asks
   before the chat's mode does and never reach the CLI's settings. */
export function PermissionsSection({
  chat,
  workspace,
  siblings,
  disabled = false,
  onChanged,
}: {
  chat: Chat;
  workspace?: Environment;
  siblings: Chat[];
  disabled?: boolean;
  onChanged: () => void;
}) {
  const [kind, setKind] = useState<Rule["kind"]>("deny");
  const [pattern, setPattern] = useState("");
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const rules = workspace?.rules ?? [];
  const chats = [chat, ...siblings].filter((c) => c.rules?.length);
  const titles = (workspace?.chats ?? [chat, ...siblings]).map((c) => ({
    id: c.id,
    title: c.title,
  }));
  async function act(what: string, path: string, body: unknown) {
    setBusy(what);
    setError("");
    try {
      await api(path, body);
      onChanged();
      return true;
    } catch (e) {
      setError(String(e));
      return false;
    } finally {
      setBusy("");
    }
  }
  async function add(event: FormEvent) {
    event.preventDefault();
    const problem = ruleError(pattern);
    if (problem) {
      setError(problem);
      return;
    }
    if (
      await act("add", `environments/${chat.sandboxID}/rules`, {
        kind,
        pattern: pattern.trim(),
      })
    )
      setPattern("");
  }
  const item = (r: Rule, path: string) => (
    <li key={r.id} className="rule-row">
      <span className={`rule-kind rule-${r.kind}`} title={kindHint(r.kind)}>
        {r.kind}
      </span>
      <code title={r.pattern}>{r.pattern}</code>
      <small title={r.at ? new Date(r.at * 1000).toLocaleString() : ""}>
        {ruleOrigin(r, titles)}
      </small>
      {!disabled && (
        <button
          className="ghost"
          disabled={!!busy}
          aria-label={`Remove rule ${r.pattern}`}
          onClick={() => void act(r.id, `${path}/${r.id}/remove`, {})}
        >
          Remove
        </button>
      )}
    </li>
  );
  return (
    <section className="workspace-section workspace-permissions">
      <h2>Permissions</h2>
      <p className="muted">
        Rules answer Claude's tool asks before the chat's mode does, in every
        chat of this workspace: <em>deny</em> refuses without asking in every
        mode, <em>ask</em> makes a card even in auto, <em>allow</em> answers
        in ask mode.
      </p>
      {!rules.length && <p className="muted">No workspace rules.</p>}
      <ul>{rules.map((r) => item(r, `environments/${chat.sandboxID}/rules`))}</ul>
      {!disabled && (
        <form className="rule-form" onSubmit={add}>
          <select
            aria-label="Rule kind"
            value={kind}
            disabled={!!busy}
            onChange={(e) => setKind(e.target.value as Rule["kind"])}
          >
            {RULE_KINDS.map((k) => (
              <option key={k.kind} value={k.kind} title={k.hint}>
                {k.kind}
              </option>
            ))}
          </select>
          <input
            aria-label="Rule pattern"
            list="rule-examples"
            placeholder="Bash(git *), Edit(src/**), Read…"
            value={pattern}
            disabled={!!busy}
            maxLength={512}
            onChange={(e) => {
              setPattern(e.target.value);
              if (error) setError("");
            }}
          />
          <datalist id="rule-examples">
            {RULE_EXAMPLES.map((x) => (
              <option key={x} value={x} />
            ))}
          </datalist>
          <button
            className="primary"
            disabled={!!busy || !pattern.trim()}
            title="Add the rule to every chat of this workspace"
          >
            Add
          </button>
        </form>
      )}
      {chats.map((c) => (
        <div key={c.id} className="rule-chat">
          <h3>
            {c.id === chat.id ? "This chat" : c.title}
            <small>allowed always in this chat only</small>
          </h3>
          <ul>{c.rules!.map((r) => item(r, `chats/${c.id}/rules`))}</ul>
        </div>
      ))}
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
    </section>
  );
}
