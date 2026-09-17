import type { ModelOption } from "../composer";

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

/* The choices the picker offers for a provider, the default first; the
   composer's /model command lists the same. */
export function modelOptions(provider: string): ModelOption[] {
  return [
    { value: "", label: "Provider default" },
    ...(models[provider] || []),
  ];
}

export function ModelSelect({
  provider,
  value,
  disabled,
  onChange,
  label,
}: {
  provider: string;
  value: string;
  disabled?: boolean;
  onChange: (value: string) => void;
  label: string;
}) {
  const options = models[provider] || [];
  return (
    <select
      aria-label={label}
      value={value}
      disabled={disabled}
      onChange={(e) => onChange(e.target.value)}
    >
      <option value="">Provider default</option>
      {value && !options.some((option) => option.value === value) && (
        <option value={value}>{value} (current)</option>
      )}
      {options.map((option) => (
        <option key={option.value} value={option.value}>
          {option.label}
        </option>
      ))}
    </select>
  );
}
