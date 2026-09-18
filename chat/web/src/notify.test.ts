import { describe, it, expect } from "vitest";
import {
  approvalBody,
  lastReply,
  notifyEvents,
  readNotifyPreference,
  shouldNotify,
  writeNotifyPreference,
} from "./notify";
import { badgeText } from "./favicon";
import type { Approval, Chat, Entry } from "./types";

const entry = (id: string, role: string, text: string, extra: Partial<Entry> = {}): Entry => ({
  id,
  role,
  text,
  detail: "",
  createdAt: 1,
  isStreaming: false,
  delivery: "sent",
  ...extra,
});
const chat = (over: Partial<Chat> = {}): Chat => ({
  id: "c1",
  title: "Fix the build",
  sandboxID: "s",
  repository: "",
  status: "idle",
  archived: false,
  conversation: { entries: [] },
  approvals: [],
  ...over,
});
const ask = (id: string, params: Approval["params"]): Approval => ({
  id,
  method: "item/tool/requestPermission",
  state: "pending",
  params,
});

describe("notifyEvents", () => {
  it("notices a turn ending, with the agent's last words", () => {
    const before = [chat({ status: "running" })];
    const after = [
      chat({
        status: "idle",
        conversation: {
          entries: [
            entry("u", "user", "do it"),
            entry("a", "assistant", "Done: the build passes now.\nAll green."),
          ],
        },
      }),
    ];
    expect(notifyEvents(before, after)).toEqual([
      {
        kind: "completed",
        chatID: "c1",
        title: "Fix the build: the agent finished",
        body: "Done: the build passes now. All green.",
        tag: "c1:completed",
      },
    ]);
  });
  it("notices a failure and a stop, not a start", () => {
    expect(
      notifyEvents([chat({ status: "running" })], [chat({ status: "failed", error: "boom" })]),
    ).toMatchObject([{ kind: "failed", body: "boom" }]);
    // An interruption the person asked for is not news.
    expect(notifyEvents([chat({ status: "running" })], [chat({ status: "interrupted" })])).toEqual([]);
    expect(notifyEvents([chat({ status: "idle" })], [chat({ status: "running" })])).toEqual([]);
    expect(notifyEvents([chat({ status: "running" })], [chat({ status: "queued" })])).toEqual([]);
  });
  it("notices a new pending approval once", () => {
    const a = ask("ap1", { tool: "Bash", description: "run the tests", input: {} });
    const events = notifyEvents([chat()], [chat({ approvals: [a] })]);
    expect(events).toMatchObject([
      { kind: "approval", body: "Bash: run the tests", tag: "c1:approval:ap1" },
    ]);
    expect(notifyEvents([chat({ approvals: [a] })], [chat({ approvals: [a] })])).toEqual([]);
    const answered = { ...a, state: "answered" };
    expect(notifyEvents([chat({ approvals: [a] })], [chat({ approvals: [answered] })])).toEqual([]);
  });
  it("ignores chats it has not seen before and the first state", () => {
    expect(notifyEvents(undefined, [chat({ status: "idle" })])).toEqual([]);
    expect(notifyEvents([], [chat({ status: "idle" })])).toEqual([]);
  });
  it("words what the agent asks", () => {
    expect(approvalBody(ask("a", { tool: "ExitPlanMode", input: {} }))).toBe("Claude proposes a plan");
    expect(approvalBody(ask("a", { tool: "Edit", entry: entry("e", "activity", "Edit main.go") as Entry, input: {} }))).toBe(
      "Edit: Edit main.go",
    );
    expect(
      approvalBody({ id: "q", method: "item/tool/requestUserInput", state: "pending", params: { questions: [{ id: "q0", question: "Which port?" }] } }),
    ).toBe("Which port?");
    expect(
      approvalBody({ id: "n", method: "warden/network", state: "pending", params: { reason: "to fetch the docs" } }),
    ).toBe("to fetch the docs");
    expect(approvalBody({ id: "x", method: "other", state: "pending", params: {} })).toBe("An approval is waiting");
    expect(lastReply([entry("a", "assistant", "x".repeat(200))])).toHaveLength(141);
    expect(lastReply([entry("a", "assistant", "child", { parentID: "task" })])).toBe("The agent's turn is over");
  });
});

describe("the preference and the decision", () => {
  it("keeps the choice in storage", () => {
    const store = new Map<string, string>();
    const storage = {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => void store.set(k, v),
    };
    expect(readNotifyPreference(storage)).toBe("unset");
    writeNotifyPreference(storage, true);
    expect(readNotifyPreference(storage)).toBe("on");
    writeNotifyPreference(storage, false);
    expect(readNotifyPreference(storage)).toBe("off");
    expect(readNotifyPreference(undefined)).toBe("unset");
    const broken = {
      getItem: () => {
        throw new Error("private mode");
      },
    };
    expect(readNotifyPreference(broken)).toBe("unset");
  });
  it("notifies only when wanted, allowed and the tab is hidden", () => {
    expect(shouldNotify("on", "granted", true)).toBe(true);
    expect(shouldNotify("on", "granted", false)).toBe(false);
    expect(shouldNotify("on", "denied", true)).toBe(false);
    expect(shouldNotify("off", "granted", true)).toBe(false);
    expect(shouldNotify("unset", "granted", true)).toBe(false);
  });
  it("counts on the badge up to nine", () => {
    expect(badgeText(0)).toBe("");
    expect(badgeText(3)).toBe("3");
    expect(badgeText(12)).toBe("9+");
  });
});
