// The context meter in the composer footer: how much of the model's window
// the conversation takes, with a tick where Claude compacts on its own
// (the window less its buffer) when it has said; amber from 80 % of the
// way there, red from 95 %. The title says the exact counts and what to
// expect.
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
          {!!context.threshold && context.threshold < context.window && (
            <i
              className="context-threshold"
              style={{ left: `${(context.threshold / context.window) * 100}%` }}
            />
          )}
        </span>
      )}
      <span>{s.label}</span>
    </span>
  );
}
