// The context meter in the composer footer: how much of the model's window
// the conversation takes, amber from 80 %, red from 95 % (Claude compacts
// on its own a little under the window). The title says the exact counts
// and what to expect.
import { contextSummary } from "../context";
import type { Context } from "../types";

export function ContextMeter({ context }: { context: Context }) {
  const s = contextSummary(context);
  return (
    <span
      className={`context-meter ${s.level}`}
      title={s.title}
      aria-label={`Context ${s.label}${context.window > 0 ? ` (${s.percent} %)` : ""}`}
    >
      {context.window > 0 && (
        <span className="context-bar" aria-hidden="true">
          <span style={{ width: `${Math.max(2, s.fraction * 100)}%` }} />
        </span>
      )}
      <span>{s.label}</span>
    </span>
  );
}
