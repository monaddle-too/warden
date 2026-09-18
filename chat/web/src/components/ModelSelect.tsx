import { THINKING, thinkingLabel } from "../composer";
import type { ModelOption } from "../composer";
import { effortOptions, fastModeFor, modelOptions } from "../models";
import type { AgentOptions, SessionSettings } from "../types";

export { modelOptions } from "../models";

/* The model picker, and on Claude chats the session settings beside it:
   the thinking budget, the effort level and, when the service allows it,
   fast mode (chats/settings.go; /thinking and /effort set the same). The
   model the agent's session reports (chat.session.model) is the truth
   after a live switch and is shown on the chosen option. */
export function ModelSelect({
  provider,
  value,
  disabled,
  onChange,
  label,
  session,
  settings,
  options,
  onSettings,
}: {
  provider: string;
  value: string;
  disabled?: boolean;
  onChange: (value: string) => void;
  label: string;
  session?: { model?: string; fastMode?: string };
  settings?: SessionSettings;
  options?: AgentOptions;
  onSettings?: (change: SessionSettings) => void;
}) {
  const rows = modelOptions(provider, options);
  const choices = rows.slice(1);
  const reported = session?.model;
  const withReported = (option: ModelOption) =>
    reported && option.value === value
      ? `${option.label} · ${reported}`
      : option.label;
  const claude = provider === "claude" && settings && onSettings;
  const thinking = settings?.thinking || "";
  const effort = settings?.effort || "";
  const efforts = effortOptions(provider, value, options);
  // A model the catalog says takes no effort level: the select shows the
  // default alone and says so.
  const noEffort = efforts.length === 1;
  const fast = fastModeFor(provider, value, options, session?.fastMode);
  const chosen = rows.find((option) => option.value === value);
  return (
    <>
      <select
        aria-label={label}
        title={
          reported ? `The session runs ${reported}` : chosen?.hint || undefined
        }
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
      >
        <option value="" title={rows[0].hint}>
          {withReported(rows[0])}
        </option>
        {value && !choices.some((option) => option.value === value) && (
          <option value={value}>{value} (current)</option>
        )}
        {choices.map((option) => (
          <option
            key={option.value}
            value={option.value}
            title={option.hint}
            disabled={option.disabled && option.value !== value}
          >
            {option.disabled
              ? `${option.label} (not allowed)`
              : withReported(option)}
          </option>
        ))}
      </select>
      {claude && (
        <>
          <select
            aria-label="Thinking"
            title="How much the model thinks before answering; /thinking sets any budget"
            value={thinking}
            disabled={disabled}
            onChange={(e) => onSettings({ thinking: e.target.value })}
          >
            {!THINKING.some((t) => t.value === thinking) && (
              <option value={thinking}>
                Thinking {thinkingLabel(thinking)}
              </option>
            )}
            {THINKING.map((t) => (
              <option key={t.value} value={t.value} title={t.hint}>
                {t.label}
              </option>
            ))}
          </select>
          <select
            aria-label="Effort"
            title={
              noEffort
                ? "The chosen model takes no effort level"
                : "How hard the model works on each answer"
            }
            value={effort}
            disabled={disabled || (noEffort && !effort)}
            onChange={(e) => onSettings({ effort: e.target.value })}
          >
            {effort && !efforts.some((e) => e.value === effort) && (
              <option value={effort}>Effort {effort}</option>
            )}
            {efforts.map((e) => (
              <option key={e.value} value={e.value} title={e.hint}>
                {e.label}
              </option>
            ))}
          </select>
          <label
            className={"composer-fast" + (fast.allowed ? "" : " disallowed")}
            title={fast.title}
          >
            <input
              type="checkbox"
              checked={!!settings?.fast}
              disabled={disabled || !fast.allowed}
              onChange={(e) => onSettings({ fast: e.target.checked })}
            />
            Fast
          </label>
        </>
      )}
    </>
  );
}
