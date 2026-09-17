import { useState } from "react";
import { ShieldAlert } from "lucide-react";
import type { Approval } from "../types";
import { api } from "../api";
/* Top-level parameters shown as label/value rows; nested values are
   summarised and the full request stays behind "Show raw request". */
// Owner-approved grants an agent can request (see chats/grants.go): what
// the card says and what the primary button does.
function describeGrant(approval: Approval) {
  const p = approval.params as Record<string, unknown>;
  const s = (k: string) => (p[k] == null ? "" : String(p[k]));
  const reason = s("reason");
  switch (approval.method) {
    case "warden/network/allow":
      return {
        title: `Allow network access to ${s("host")}`,
        text: `This sandbox could reach ${s("host")} over HTTP and HTTPS for ${s("duration_minutes")} minutes, through Warden's gateway, with no credential attached. Other sandboxes are unaffected.`,
        reason,
        button: "Allow for " + s("duration_minutes") + " min",
      };
    case "warden/repository/access":
      return {
        title: `Share ${s("repository")} with this workspace`,
        text: `Read-only access to ${(p.categories as string[] | undefined)?.map((c) => c.replace("pull_requests", "pull requests").replace("contents", "code")).join(", ")} of ${s("repository")}, for every chat in this workspace until you remove it. Writes still need their own approval.`,
        reason,
        button: "Share repository",
      };
    case "warden/github/write": {
      const action = s("action");
      const what =
        action === "create_issue"
          ? `Open an issue in ${s("repository")}: “${s("title")}”`
          : action === "add_labels"
            ? `Add labels ${(p.labels as string[] | undefined)?.join(", ")} to #${s("number")} in ${s("repository")}`
            : `Comment on #${s("number")} in ${s("repository")}`;
      return {
        title: what,
        text: "Warden posts exactly this with your GitHub credential. The agent never holds a token.",
        body: action === "add_labels" ? "" : s("body"),
        reason: "",
        button:
          action === "create_issue"
            ? "Open issue"
            : action === "add_labels"
              ? "Add labels"
              : "Post comment",
      };
    }
    case "warden/host/import":
      return {
        title: `Copy ${s("path")} into the sandbox`,
        text: "A snapshot of this directory from your computer is copied into the sandbox at /home/agent/host, up to 1 GiB. Your original is not touched; the agent can ask later to copy its changes back.",
        reason,
        button: "Copy directory in",
      };
    case "warden/host/export":
      return {
        title: `Copy the sandbox's files back over ${s("path")}`,
        text: "Files under the sandbox's copy overwrite the same paths in this directory on your computer. Nothing is deleted; files only in your directory stay.",
        reason: "",
        button: "Copy changes back",
      };
  }
  return undefined;
}

function summarize(value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean")
    return String(value);
  if (Array.isArray(value))
    return value.every((v) => typeof v !== "object")
      ? value.map(summarize).join(", ")
      : `${value.length} item${value.length === 1 ? "" : "s"}`;
  const keys = Object.keys(value as object);
  return `{ ${keys.slice(0, 4).join(", ")}${keys.length > 4 ? ", …" : ""} }`;
}
export function ApprovalCard({
  chatID,
  approval,
}: {
  chatID: string;
  approval: Approval;
}) {
  const [answers, setAnswers] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function resolve(allow: boolean) {
    setBusy(true);
    setError("");
    try {
      await api(`chats/${chatID}/approvals/${approval.id}`, {
        allow,
        answers: Object.fromEntries(
          Object.entries(answers).map(([id, text]) => [id, [text]]),
        ),
      });
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  const questions = approval.params.questions;
  const port = approval.method === "warden/ports/bind";
  const grant = describeGrant(approval);
  const rows = Object.entries(approval.params).filter(
    ([key]) => key !== "questions",
  );
  return (
    <section className="approval-card" aria-label="Approval request">
      <div className="approval-head">
        <span className="approval-icon" aria-hidden="true">
          <ShieldAlert size={18} />
        </span>
        <div>
          <h3>
            {port
              ? "Bind sandbox port " + String(approval.params.port)
              : grant
                ? grant.title
                : questions
                  ? "The agent has a question"
                  : "Approval requested"}
          </h3>
          {!questions && !grant && (
            <p>
              <code>{approval.method}</code>
            </p>
          )}
        </div>
      </div>
      {questions ? (
        questions.map((q) => (
          <label key={q.id}>
            {q.question}
            {!!q.options?.length && (
              <span className="approval-options">
                {q.options.map((o) => (
                  <button
                    type="button"
                    key={o.label}
                    title={o.description}
                    aria-pressed={answers[q.id] === o.label}
                    onClick={() => setAnswers({ ...answers, [q.id]: o.label })}
                  >
                    {o.label}
                  </button>
                ))}
              </span>
            )}
            <input
              aria-label={q.question}
              placeholder="Your answer"
              value={answers[q.id] || ""}
              onChange={(e) =>
                setAnswers({ ...answers, [q.id]: e.target.value })
              }
            />
          </label>
        ))
      ) : port ? (
        <p>
          Make {String(approval.params.title)} reachable at a Warden URL. Only
          signed-in Warden users can access it. The binding lasts until you
          revoke it; stopping the sandbox makes it unavailable.
        </p>
      ) : grant ? (
        <>
          <p>{grant.text}</p>
          {grant.body && <pre className="approval-body">{grant.body}</pre>}
          {grant.reason && (
            <p className="muted">Agent's reason: {grant.reason}</p>
          )}
        </>
      ) : (
        <>
          {rows.length > 0 && (
            <dl className="approval-params">
              {rows.map(([key, value]) => (
                <div key={key} style={{ display: "contents" }}>
                  <dt>{key}</dt>
                  <dd>{summarize(value)}</dd>
                </div>
              ))}
            </dl>
          )}
          <details>
            <summary>Show raw request</summary>
            <pre>{JSON.stringify(approval.params, null, 2)}</pre>
          </details>
        </>
      )}
      <div className="approval-actions">
        <button disabled={busy} onClick={() => resolve(false)}>
          Decline
        </button>
        <button
          className="primary"
          disabled={
            busy ||
            (!!questions && questions.some((q) => !answers[q.id]?.trim()))
          }
          onClick={() => resolve(true)}
        >
          {port
            ? "Bind port"
            : grant
              ? grant.button
              : questions
                ? "Send answer"
                : "Allow once"}
        </button>
      </div>
      {error && (
        <p role="alert" className="error">
          {error}
        </p>
      )}
    </section>
  );
}
