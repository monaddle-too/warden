import { describe, it, expect } from "vitest";
import {
  FAST_MODE_HINT,
  LONG_CONTEXT_HINT,
  catalogRow,
  chatEffort,
  chatModel,
  defaultModel,
  effortOptions,
  fastModeFor,
  modelOptions,
} from "./models";
import type { AgentOptions, CatalogModel } from "./types";

const catalog: CatalogModel[] = [
  {
    value: "default",
    resolved: "claude-sonnet-5",
    label: "Default (recommended)",
    description: "Sonnet 5 · Efficient for routine tasks",
    efforts: ["low", "medium", "high", "xhigh", "max"],
    adaptiveThinking: true,
  },
  {
    value: "sonnet",
    resolved: "claude-sonnet-5",
    label: "Sonnet",
    description: "Sonnet 5 · Efficient for routine tasks",
    efforts: ["low", "medium", "high", "xhigh", "max"],
    adaptiveThinking: true,
  },
  {
    value: "opus[1m]",
    resolved: "claude-opus-5[1m]",
    label: "Opus (1M context)",
    efforts: ["low", "max"],
    adaptiveThinking: true,
    fastMode: true,
  },
  { value: "haiku", resolved: "claude-haiku-4-5-20251001", label: "Haiku" },
];
const withCatalog: AgentOptions = {
  fastMode: false,
  longContext: false,
  models: { claude: catalog },
};

describe("modelOptions", () => {
  it("builds the picker's rows from the catalog, the provider's default first", () => {
    // The CLI's own "default" row is not offered; Warden's default (Opus,
    // which this catalog does not list) leads as its own row.
    const rows = modelOptions("claude", withCatalog);
    expect(rows.map((r) => r.value)).toEqual([
      "opus",
      "sonnet",
      "opus[1m]",
      "haiku",
    ]);
    expect(rows[0]).toMatchObject({ label: "Opus", hint: "Default" });
    expect(rows[1].label).toBe("Sonnet");
    expect(rows[1].hint).toBe(
      "claude-sonnet-5 · Sonnet 5 · Efficient for routine tasks",
    );
    expect(rows[1].disabled).toBeUndefined();
    expect(rows[3].hint).toBe("claude-haiku-4-5-20251001");
  });
  it("leads with the service's default when it names one, marked", () => {
    const rows = modelOptions("claude", {
      ...withCatalog,
      defaults: { claude: "sonnet" },
    });
    expect(rows.map((r) => r.value)).toEqual(["sonnet", "opus[1m]", "haiku"]);
    expect(rows[0].hint).toMatch(/^Default · claude-sonnet-5/);
    expect(defaultModel("claude", withCatalog)).toBe("opus");
    expect(defaultModel("codex")).toBe("gpt-5.6-sol");
    expect(chatModel("claude", "", withCatalog)).toBe("opus");
    expect(chatModel("claude", "haiku", withCatalog)).toBe("haiku");
  });
  it("keeps a 1M row visible but disabled with the hint until the operator allows it", () => {
    const rows = modelOptions("claude", withCatalog);
    expect(rows[2]).toMatchObject({
      value: "opus[1m]",
      label: "Opus (1M context)",
      disabled: true,
      hint: LONG_CONTEXT_HINT,
    });
    const allowed = modelOptions("claude", {
      ...withCatalog,
      longContext: true,
    });
    expect(allowed[2].disabled).toBeUndefined();
    expect(allowed[2].hint).toContain("claude-opus-5[1m]");
  });
  it("falls back to the static rows without a catalog", () => {
    expect(modelOptions("claude").map((r) => r.value)).toEqual([
      "opus",
      "sonnet",
      "haiku",
      "sonnet[1m]",
      "opus[1m]",
    ]);
    expect(
      modelOptions("claude")
        .filter((r) => r.disabled)
        .map((r) => r.value),
    ).toEqual(["sonnet[1m]", "opus[1m]"]);
    expect(
      modelOptions("claude", { fastMode: false, longContext: true }).some(
        (r) => r.disabled,
      ),
    ).toBe(false);
    expect(modelOptions("codex", withCatalog)).toHaveLength(5);
    expect(modelOptions("codex")[0]).toMatchObject({
      value: "gpt-5.6-sol",
      label: "GPT-5.6 Sol",
      hint: "Default · gpt-5.6-sol",
    });
    expect(modelOptions("codex")[1]).toMatchObject({
      value: "gpt-6-astra",
      label: "GPT-6 Astra",
    });
    expect(modelOptions("other")).toHaveLength(0);
  });
});

describe("effortOptions and fastModeFor", () => {
  it("offers each model the effort levels the catalog says it takes", () => {
    expect(
      effortOptions("claude", "sonnet", withCatalog).map((e) => e.value),
    ).toEqual(["low", "medium", "high", "xhigh", "max"]);
    expect(
      effortOptions("claude", "opus[1m]", withCatalog).map((e) => e.value),
    ).toEqual(["low", "max"]);
    expect(
      effortOptions("claude", "haiku", withCatalog).map((e) => e.value),
    ).toEqual([]);
    // Unknown to the catalog, or no catalog: every level; no default row.
    expect(effortOptions("claude", "opusplan", withCatalog)).toHaveLength(5);
    expect(effortOptions("claude", "haiku")).toHaveLength(5);
    expect(effortOptions("claude", "", withCatalog)).toHaveLength(5);
    // A chat without a model runs the default; "" resolves to it.
    expect(
      catalogRow("claude", "", { ...withCatalog, defaults: { claude: "sonnet" } })
        ?.value,
    ).toBe("sonnet");
    expect(catalogRow("claude", "haiku")).toBeUndefined();
    expect(chatEffort("")).toBe("high");
    expect(chatEffort("low")).toBe("low");
  });
  it("disables fast mode with the config hint until allowed, and says when the model lacks it", () => {
    const off = fastModeFor("claude", "opus[1m]", withCatalog);
    expect(off.allowed).toBe(false);
    expect(off.supported).toBe(true);
    expect(off.title).toContain(FAST_MODE_HINT);
    const on = fastModeFor(
      "claude",
      "opus[1m]",
      { ...withCatalog, fastMode: true },
      "on",
    );
    expect(on.allowed).toBe(true);
    expect(on.title).not.toContain(FAST_MODE_HINT);
    expect(on.title).toContain("now on");
    const sonnet = fastModeFor("claude", "sonnet", {
      ...withCatalog,
      fastMode: true,
    });
    expect(sonnet.supported).toBe(false);
    expect(sonnet.title).toContain("not offered on Sonnet");
    expect(
      fastModeFor("claude", "sonnet", { fastMode: true, longContext: false })
        .supported,
    ).toBeUndefined();
  });
});
