import { STYLES } from "../composer";

/* A Claude chat's output style, beside the model (composer.ts STYLES;
   the service's chats/style.go). It is a launch setting: the chat's idle
   session ends when it changes and the next message starts one with
   the style, so the title says what the running session has when that
   differs. Codex chats have no style; the shell hides the selector. */
export function StyleSelect({
  value,
  running,
  disabled,
  onChange,
}: {
  value: string;
  /* What the agent's current session reported, when it has. */
  running?: string;
  disabled?: boolean;
  onChange: (value: string) => void;
}) {
  const current = STYLES.find((s) => s.value === (value || ""));
  const live = running && running !== "default" ? running : "";
  const pending = !!running && live !== (value || "");
  const title = [
    `Output style: ${current?.label || value}. ${current?.hint || ""}`.trim(),
    "Applies when the next session starts.",
    pending ? `The running session uses ${live || "the default"}.` : "",
  ]
    .filter(Boolean)
    .join(" ");
  return (
    <select
      aria-label="Output style"
      className={pending ? "style-pending" : undefined}
      title={title}
      value={value || ""}
      disabled={disabled}
      onChange={(e) => onChange(e.target.value)}
    >
      {STYLES.map((style) => (
        <option key={style.value} value={style.value} title={style.hint}>
          {style.label}
          {pending && style.value === (value || "") ? " (next session)" : ""}
        </option>
      ))}
    </select>
  );
}
