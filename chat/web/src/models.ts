/* The model picker's rows (docs/claude-parity.md, R2.3): from the CLI's
   catalog when the service reported one (agentOptions.models, the
   engine's copy of Claude Code's list_models: each row with its effort
   levels and whether it has fast mode), else the static rows that served
   before. A 1M-context row, and fast mode, that the operator has not
   allowed (providers.claude.allowLongContext / allowFastMode) stay
   listed but disabled, with the hint naming the switch, instead of
   vanishing. Codex keeps its static rows: its app-server offers no
   catalog. The TUI's complete.go does the same. */
import { EFFORTS } from "./composer";
import type { EffortOption, ModelOption } from "./composer";
import type { AgentOptions, CatalogModel } from "./types";

const staticModels: Record<string, CatalogModel[]> = {
  codex: [
    { value: "gpt-6-astra", label: "GPT-6 Astra" },
    { value: "gpt-5.6-sol", label: "GPT-5.6 Sol" },
    { value: "gpt-5.6-terra", label: "GPT-5.6 Terra" },
    { value: "gpt-5.6-luna", label: "GPT-5.6 Luna" },
    { value: "gpt-5.5", label: "GPT-5.5" },
  ],
  claude: [
    { value: "sonnet", label: "Sonnet", adaptiveThinking: true },
    { value: "opus", label: "Opus", adaptiveThinking: true, fastMode: true },
    { value: "haiku", label: "Haiku" },
    { value: "sonnet[1m]", label: "Sonnet 1M", adaptiveThinking: true },
    {
      value: "opus[1m]",
      label: "Opus 1M",
      adaptiveThinking: true,
      fastMode: true,
    },
  ],
};

export const LONG_CONTEXT_HINT =
  "1M context costs more per token; enable providers.claude.allowLongContext in warden.json to offer it";
export const FAST_MODE_HINT =
  "Fast mode costs more; enable providers.claude.allowFastMode in warden.json to offer it";

/* A 1M-context alias (the service's longContextModel). */
export function isLongContext(value: string): boolean {
  return value.endsWith("[1m]");
}

/* The provider's catalog rows, or the static ones. */
export function catalogRows(
  provider: string,
  options?: AgentOptions,
): CatalogModel[] {
  const rows = options?.models?.[provider];
  return rows && rows.length ? rows : staticModels[provider] || [];
}

/* The catalog row a chat's model means: its own, or the "default" row
   for the provider default; undefined without a catalog or a match. */
export function catalogRow(
  provider: string,
  model: string,
  options?: AgentOptions,
): CatalogModel | undefined {
  const rows = options?.models?.[provider];
  if (!rows?.length) return undefined;
  return rows.find((r) => r.value === (model || "default"));
}

/* The choices the picker offers for a provider, the default first; the
   composer's /model command lists the same. */
export function modelOptions(
  provider: string,
  options?: AgentOptions,
): ModelOption[] {
  const rows = catalogRows(provider, options);
  const fallback = rows.find((r) => r.value === "default");
  const out: ModelOption[] = [
    {
      value: "",
      label: "Provider default",
      hint: describe(fallback) || "The provider's default model",
    },
  ];
  for (const r of rows) {
    if (r.value === "default") continue;
    const option: ModelOption = {
      value: r.value,
      label: provider === "claude" ? `Claude ${r.label}` : r.label,
      hint: describe(r) || r.value,
    };
    if (
      provider === "claude" &&
      isLongContext(r.value) &&
      !options?.longContext
    ) {
      option.disabled = true;
      option.hint = LONG_CONTEXT_HINT;
    }
    out.push(option);
  }
  return out;
}

function describe(r?: CatalogModel): string {
  if (!r) return "";
  return [r.resolved, r.description].filter(Boolean).join(" · ");
}

/* The effort rows for a chat's model: the default row, then the levels
   the catalog says the model takes (every level without a catalog; none
   for a model like Haiku, which takes no effort level). */
export function effortOptions(
  provider: string,
  model: string,
  options?: AgentOptions,
): EffortOption[] {
  const row = catalogRow(provider, model, options);
  if (!row) return EFFORTS;
  const levels = row.efforts || [];
  return EFFORTS.filter((e) => e.value === "" || levels.includes(e.value));
}

/* Fast mode for a chat's model: whether the operator allows it (the
   checkbox is disabled with the hint otherwise), whether the catalog says
   the model has it (undefined without a catalog), and the title. */
export function fastModeFor(
  provider: string,
  model: string,
  options?: AgentOptions,
  sessionState?: string,
): { allowed: boolean; supported?: boolean; title: string } {
  const allowed = !!options?.fastMode;
  const row = catalogRow(provider, model, options);
  const supported = row ? !!row.fastMode : undefined;
  const parts = [
    "Fast mode: quicker answers at a higher price, on the models that offer it",
  ];
  if (!allowed) parts.push(FAST_MODE_HINT);
  else if (supported === false)
    parts.push(`not offered on ${row?.label || "this model"}`);
  if (sessionState) parts.push(`now ${sessionState}`);
  return { allowed, supported, title: parts.join(" · ") };
}
