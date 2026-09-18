import { EFFORTS, THINKING, thinkingLabel } from "../composer";
import type { ModelOption } from "../composer";
import type { AgentOptions, SessionSettings } from "../types";

const models: Record<string, ModelOption[]> = {
  codex: [
    { value: "gpt-6-astra", label: "GPT-6 Astra" },
    { value: "gpt-5.6-sol", label: "GPT-5.6 Sol" },
    { value: "gpt-5.6-terra", label: "GPT-5.6 Terra" },
    { value: "gpt-5.6-luna", label: "GPT-5.6 Luna" },
    { value: "gpt-5.5", label: "GPT-5.5" },
  ],
  claude: [
    { value: "sonnet", label: "Claude Sonnet" },
    { value: "opus", label: "Claude Opus" },
    { value: "haiku", label: "Claude Haiku" },
  ],
};

/* The 1M-context variants, offered when the service allows them
   (agentOptions.longContext): they cost more per token. */
const longContext: ModelOption[] = [
  { value: "sonnet[1m]", label: "Claude Sonnet 1M" },
  { value: "opus[1m]", label: "Claude Opus 1M" },
];

/* The choices the picker offers for a provider, the default first; the
   composer's /model command lists the same. */
export function modelOptions(
  provider: string,
  options?: AgentOptions,
): ModelOption[] {
  return [
    { value: "", label: "Provider default" },
    ...(models[provider] || []),
    ...(provider === "claude" && options?.longContext ? longContext : []),
  ];
}

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
  const choices = modelOptions(provider, options).slice(1);
  const reported = session?.model;
  const withReported = (option: ModelOption) =>
    reported && option.value === value
      ? `${option.label} · ${reported}`
      : option.label;
  const claude = provider === "claude" && settings && onSettings;
  const thinking = settings?.thinking || "";
  const effort = settings?.effort || "";
  return (
    <>
      <select
        aria-label={label}
        title={reported ? `The session runs ${reported}` : undefined}
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
      >
        <option value="">
          {withReported({ value: "", label: "Provider default" })}
        </option>
        {value && !choices.some((option) => option.value === value) && (
          <option value={value}>{value} (current)</option>
        )}
        {choices.map((option) => (
          <option key={option.value} value={option.value}>
            {withReported(option)}
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
            title="How hard the model works on each answer"
            value={effort}
            disabled={disabled}
            onChange={(e) => onSettings({ effort: e.target.value })}
          >
            {EFFORTS.map((e) => (
              <option key={e.value} value={e.value} title={e.hint}>
                {e.label}
              </option>
            ))}
          </select>
          {options?.fastMode && (
            <label
              className="composer-fast"
              title={
                "Fast mode: quicker answers at a higher price, on the models that offer it" +
                (session?.fastMode ? ` (now ${session.fastMode})` : "")
              }
            >
              <input
                type="checkbox"
                checked={!!settings?.fast}
                disabled={disabled}
                onChange={(e) => onSettings({ fast: e.target.checked })}
              />
              Fast
            </label>
          )}
        </>
      )}
    </>
  );
}
