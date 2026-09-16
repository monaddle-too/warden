import { useState } from "react";
import { ShieldAlert } from "lucide-react";
import type { Approval } from "../types";
import { api } from "../api";
/* Top-level parameters shown as label/value rows; nested values are
   summarised and the full request stays behind "Show raw request". */
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
              : questions
                ? "The agent has a question"
                : "Approval requested"}
          </h3>
          {!questions && (
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
          {port ? "Bind port" : questions ? "Send answer" : "Allow once"}
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
