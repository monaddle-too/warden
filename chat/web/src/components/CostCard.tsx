import { useEffect, useState } from "react";
import { Receipt, X } from "lucide-react";
import { costRows, sessionCost } from "../cost";
import type { Entry, Turn } from "../types";

/* The /cost card: this chat's turns, tokens and cost so far, from the
   service's turn records, shown to the reader who asked and never sent
   to the agent. A running turn keeps the time counting. */
export function CostCard({
  turns,
  entries,
  provider,
  running,
  onClose,
}: {
  turns?: Turn[];
  /* The transcript, for the side questions in it. */
  entries?: Entry[];
  provider?: string;
  running: boolean;
  onClose: () => void;
}) {
  const [now, setNow] = useState(() => Date.now() / 1000);
  useEffect(() => {
    if (!running) return;
    const id = setInterval(() => setNow(Date.now() / 1000), 1000);
    return () => clearInterval(id);
  }, [running]);
  const summary = sessionCost(turns, running ? now : undefined, entries);
  return (
    <section className="cost-card" aria-label="Chat cost">
      <header>
        <Receipt size={14} aria-hidden="true" />
        <strong>Cost so far</strong>
        <span className="muted">
          {provider === "claude" ? "Claude Code's own estimate" : "tokens as the agent reports them"}
          {" · not sent to the agent"}
        </span>
        <button
          type="button"
          className="ghost icon"
          aria-label="Dismiss"
          onClick={onClose}
        >
          <X size={14} />
        </button>
      </header>
      <table>
        <tbody>
          {costRows(summary).map(([label, value]) => (
            <tr key={label} className={label.startsWith("  ") ? "sub" : ""}>
              <th scope="row">{label.trim()}</th>
              <td>{value}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
