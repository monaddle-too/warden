import { describe, expect, it } from "vitest";
import {
  pendingReply,
  providerOf,
  thinkingLabel,
  thinkingOpen,
} from "./thinking";

const entry = (
  role: string,
  createdAt: number,
  more: Partial<{ isStreaming: boolean; endedAt: number }> = {},
) => ({
  id: role + createdAt,
  role,
  text: "",
  detail: "",
  createdAt,
  isStreaming: false,
  delivery: "",
  ...more,
});
const chat = (
  status: string,
  entries: ReturnType<typeof entry>[],
  startup?: { stage: string; since: number },
) => ({ status, startup, conversation: { entries } });

describe("thinking", () => {
  it("knows the two agents, defaulting to Codex", () => {
    expect(providerOf("claude")).toBe("claude");
    expect(providerOf("codex")).toBe("codex");
    expect(providerOf(undefined)).toBe("codex");
  });
  it("heads the disclosure the way each app does", () => {
    const streaming = { isStreaming: true, createdAt: 100 };
    expect(thinkingLabel("claude", streaming)).toBe("Thinking…");
    expect(thinkingLabel("codex", streaming)).toBe("Thinking");
    const done = { isStreaming: false, createdAt: 100, endedAt: 112 };
    expect(thinkingLabel("claude", done)).toBe("Thought for 12s");
    expect(thinkingLabel("codex", done)).toBe("Thought for 12s");
    expect(
      thinkingLabel("claude", { isStreaming: false, createdAt: 100 }),
    ).toBe("Thought");
  });
  it("opens Codex's summary while it streams and keeps Claude's folded", () => {
    expect(thinkingOpen("codex", true)).toBe(true);
    expect(thinkingOpen("codex", false)).toBe(false);
    expect(thinkingOpen("claude", true)).toBe(false);
  });
  it("awaits a reply only while a turn runs with nothing on screen for it", () => {
    const sent = [entry("user", 50)];
    expect(pendingReply(chat("running", sent), false)).toEqual({ since: 50 });
    expect(pendingReply(chat("idle", sent), false)).toBeUndefined();
    expect(pendingReply(chat("queued", sent), false)).toBeUndefined();
    expect(pendingReply(chat("running", sent), true)).toBeUndefined();
    expect(
      pendingReply(
        chat("running", [
          ...sent,
          entry("thinking", 51, { isStreaming: true }),
        ]),
        false,
      ),
    ).toBeUndefined();
    // Between a tool step and the next model call the wait is the model's.
    expect(
      pendingReply(
        chat("running", [
          ...sent,
          entry("activity", 51, { isStreaming: false }),
        ]),
        false,
      ),
    ).toEqual({ since: 50 });
  });
  it("waits for the sandbox's own stages to pass first", () => {
    const sent = [entry("user", 50)];
    expect(
      pendingReply(
        chat("running", sent, { stage: "creating", since: 0 }),
        false,
      ),
    ).toBeUndefined();
    expect(
      pendingReply(
        chat("running", sent, { stage: "sending", since: 0 }),
        false,
      ),
    ).toEqual({ since: 50 });
    expect(
      pendingReply(
        chat("running", sent, { stage: "firstResponse", since: 0 }),
        false,
      ),
    ).toEqual({ since: 50 });
  });
});
