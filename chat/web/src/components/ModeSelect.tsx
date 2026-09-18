import { MODES } from "../composer";

/* The permission mode of a Claude chat, beside the model: auto, ask or
   plan (composer.ts MODES; the service's chats/permissions.go). Codex
   chats have no mode; the shell hides the selector for them. */
export function ModeSelect({
  value,
  disabled,
  onChange,
}: {
  value: string;
  disabled?: boolean;
  onChange: (value: string) => void;
}) {
  const current = MODES.find((m) => m.value === (value || "auto"));
  return (
    <select
      aria-label="Permission mode"
      title={current?.hint}
      value={value || "auto"}
      disabled={disabled}
      onChange={(e) => onChange(e.target.value)}
    >
      {MODES.map((mode) => (
        <option key={mode.value} value={mode.value} title={mode.hint}>
          {mode.label}
        </option>
      ))}
    </select>
  );
}
