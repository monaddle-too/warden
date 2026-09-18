import { useEffect, useRef, useState } from "react";
import { Check, ChevronDown } from "lucide-react";
import { STYLES, THINKING, thinkingLabel } from "../composer";
import { isKey } from "../shortcuts";
import {
  chatEffort,
  chatModel,
  effortOptions,
  fastModeFor,
  modelOptions,
} from "../models";
import type { AgentOptions, SessionSettings } from "../types";

/* The model and effort picker in the composer's corner, as the desktop
   agent apps pair them: one button reading "Opus · High" that opens a
   menu of the models (the provider's default first; models the operator
   has not allowed are left out) and, on a Claude chat, the effort levels
   the model takes. The rarer session settings —
   the thinking budget, the output style and fast mode when the service
   allows it — sit under "Advanced" in the same menu (chats/settings.go,
   chats/style.go; /thinking, /effort and /style set the same). The model
   the agent's session reports (chat.session.model) is the truth after a
   live switch and is named in the button's title. */
export function ComposerMenu({
  provider,
  model,
  disabled,
  onModel,
  session,
  settings,
  style,
  options,
  onSettings,
  onStyle,
}: {
  provider: string;
  model: string;
  disabled?: boolean;
  onModel: (value: string) => void;
  session?: { model?: string; fastMode?: string; outputStyle?: string };
  settings?: SessionSettings;
  style?: string;
  options?: AgentOptions;
  onSettings?: (change: SessionSettings) => void;
  onStyle?: (style: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const away = (e: MouseEvent) => {
      if (root.current && !root.current.contains(e.target as Node))
        setOpen(false);
    };
    const key = (e: KeyboardEvent) => {
      if (isKey(e, "dialog-close")) setOpen(false);
    };
    document.addEventListener("mousedown", away);
    document.addEventListener("keydown", key);
    return () => {
      document.removeEventListener("mousedown", away);
      document.removeEventListener("keydown", key);
    };
  }, [open]);

  // Models the operator has not allowed (the 1M variants) stay out of
  // the menu; /model still lists them with the hint saying why.
  const current = chatModel(provider, model, options);
  const models = modelOptions(provider, options).filter(
    (m) => !m.disabled || m.value === current,
  );
  const chosen = models.find((m) => m.value === current);
  const modelLabel = chosen?.label || current;
  const claude = provider === "claude" && !!settings && !!onSettings;
  const set = (change: SessionSettings) => onSettings?.(change);
  const efforts = claude ? effortOptions(provider, current, options) : [];
  const effort = chatEffort(settings?.effort);
  const effortLabel =
    efforts.find((e) => e.value === effort)?.label ||
    (efforts.length ? effort : "");
  const thinking = settings?.thinking || "";
  const fast = fastModeFor(provider, current, options, session?.fastMode);
  const styleRow = STYLES.find((s) => s.value === (style || ""));
  const running = session?.outputStyle;
  const styleLive = running && running !== "default" ? running : "";
  const stylePending = !!running && styleLive !== (style || "");
  const advanced = claude && (settings?.fast || thinking || style);
  const title = [
    `Model ${modelLabel}` +
      (session?.model && session.model !== current
        ? ` (the session runs ${session.model})`
        : ""),
    efforts.length ? `effort ${effortLabel.toLowerCase()}` : "",
    thinking ? `thinking ${thinkingLabel(thinking)}` : "",
    settings?.fast ? "fast mode" : "",
    style ? `${style} style` : "",
  ]
    .filter(Boolean)
    .join(" · ");
  const pick = (fn: () => void) => {
    fn();
    setOpen(false);
  };
  return (
    <div className={"composer-menu" + (open ? " open" : "")} ref={root}>
      <button
        type="button"
        className="composer-menu-button"
        aria-label="Model and effort"
        aria-haspopup="menu"
        aria-expanded={open}
        title={title}
        disabled={disabled}
        onClick={() => setOpen((o) => !o)}
      >
        <span className="composer-menu-model">{modelLabel}</span>
        {efforts.length > 0 && (
          <>
            <span className="composer-menu-sep" aria-hidden="true">
              ·
            </span>
            <span className="composer-menu-effort">{effortLabel}</span>
          </>
        )}
        {advanced && (
          <span
            className="composer-menu-mark"
            aria-label="Advanced settings changed"
          />
        )}
        <ChevronDown size={14} aria-hidden="true" />
      </button>
      {open && (
        <div className="composer-menu-list" role="menu">
          <div className="composer-menu-section">Model</div>
          {models.map((m) => (
            <button
              key={m.value}
              type="button"
              role="menuitemradio"
              aria-checked={m.value === current}
              title={m.hint}
              disabled={m.disabled && m.value !== current}
              onClick={() => pick(() => onModel(m.value))}
            >
              <span className="composer-menu-check" aria-hidden="true">
                {m.value === current && <Check size={14} />}
              </span>
              <span className="composer-menu-label">
                {m.label}
                {m.disabled ? " (not allowed)" : ""}
              </span>
            </button>
          ))}
          {efforts.length > 0 && (
            <>
              <div className="composer-menu-section">Effort</div>
              {efforts.map((e) => (
                <button
                  key={e.value}
                  type="button"
                  role="menuitemradio"
                  aria-checked={e.value === effort}
                  title={e.hint}
                  onClick={() => pick(() => set({ effort: e.value }))}
                >
                  <span className="composer-menu-check" aria-hidden="true">
                    {e.value === effort && <Check size={14} />}
                  </span>
                  <span className="composer-menu-label">{e.label}</span>
                  <span className="composer-menu-hint">{e.hint}</span>
                </button>
              ))}
            </>
          )}
          {claude && (
            <details className="composer-menu-advanced">
              <summary>Advanced</summary>
              <label>
                <span>Thinking</span>
                <select
                  aria-label="Thinking"
                  title="How much the model thinks before answering; /thinking sets any budget"
                  value={thinking}
                  onChange={(e) => set({ thinking: e.target.value })}
                >
                  {!THINKING.some((t) => t.value === thinking) && (
                    <option value={thinking}>
                      {thinkingLabel(thinking)} budget
                    </option>
                  )}
                  {THINKING.map((t) => (
                    <option key={t.value} value={t.value} title={t.hint}>
                      {t.label}
                    </option>
                  ))}
                </select>
              </label>
              {onStyle && (
                <label>
                  <span>Output style</span>
                  <select
                    aria-label="Output style"
                    title={[
                      styleRow?.hint || "",
                      "Applies when the next session starts.",
                      stylePending
                        ? `The running session uses ${styleLive || "the default"}.`
                        : "",
                    ]
                      .filter(Boolean)
                      .join(" ")}
                    value={style || ""}
                    onChange={(e) => onStyle(e.target.value)}
                  >
                    {STYLES.map((s) => (
                      <option key={s.value} value={s.value} title={s.hint}>
                        {s.label}
                        {stylePending && s.value === (style || "")
                          ? " (next session)"
                          : ""}
                      </option>
                    ))}
                  </select>
                </label>
              )}
              {fast.allowed && (
                <label className="composer-menu-fast" title={fast.title}>
                  <input
                    type="checkbox"
                    checked={!!settings?.fast}
                    onChange={(e) => set({ fast: e.target.checked })}
                  />
                  <span>Fast mode</span>
                </label>
              )}
            </details>
          )}
        </div>
      )}
    </div>
  );
}
