import { describe, it, expect } from "vitest";
import {
  canEditAndResend,
  canWithdraw,
  editingScope,
  lastQueued,
  queueHeld,
  queueHint,
  queueLabel,
  queuedLast,
  queuedMessages,
} from "./queue";
import { rewindOutcome } from "./rewind";
import type { Entry } from "./types";

function entry(partial: Partial<Entry> & { id: string; role: string }): Entry {
  return {
    text: "",
    detail: "",
    createdAt: 0,
    isStreaming: false,
    delivery: "sent",
    ...partial,
  };
}
const owner = { principalID: "owner" };
const alice = { principalID: "alice" };
const entries = [
  entry({ id: "u1", role: "user", text: "first" }),
  entry({ id: "a1", role: "assistant", text: "ok" }),
  entry({ id: "t1", role: "activity", text: "Agent" }),
  entry({
    id: "p1",
    role: "user",
    text: "subagent prompt",
    parentID: "t1",
    delivery: "queued",
  }),
  entry({
    id: "q1",
    role: "user",
    text: "second",
    delivery: "queued",
    sender: { principalID: "alice" },
  }),
  entry({ id: "q2", role: "user", text: "third", delivery: "queued" }),
];
const chat = (status: string, list = entries) => ({
  status,
  archived: false,
  conversation: { entries: list },
});

describe("the queue", () => {
  it("lists the queued messages in order, a subagent's prompt aside", () => {
    expect(queuedMessages(entries).map((e) => e.id)).toEqual(["q1", "q2"]);
  });
  it("renders the queued messages last, in their order", () => {
    const list = [entries[4], entries[0], entries[5], entries[1]];
    expect(queuedLast(list).map((e) => e.id)).toEqual(["u1", "a1", "q1", "q2"]);
    // Nothing queued: the same list, so memoised renders keep their keys.
    const plain = entries.slice(0, 2);
    expect(queuedLast(plain)).toBe(plain);
  });
  it("is held once nothing runs", () => {
    expect(queueHeld(chat("running"))).toBe(false);
    expect(queueHeld(chat("queued"))).toBe(false);
    expect(queueHeld(chat("interrupted"))).toBe(true);
    expect(queueHeld(chat("idle"))).toBe(true);
    expect(queueHeld(chat("idle", entries.slice(0, 2)))).toBe(false);
  });
  it("labels a queued message by what happens to it", () => {
    expect(queueLabel(chat("running"))).toMatch(/^Queued · will send/);
    expect(queueLabel(chat("queued"))).toBe("Queued · next up");
    expect(queueLabel(chat("interrupted"))).toMatch(/^Held/);
  });
  it("lets the sender or the owner withdraw", () => {
    const [q1, q2] = queuedMessages(entries);
    expect(canWithdraw(q1, alice)).toBe(true);
    expect(canWithdraw(q1, { principalID: "bob" })).toBe(false);
    expect(canWithdraw(q1, owner)).toBe(true);
    expect(canWithdraw(q2, alice)).toBe(false);
    expect(canWithdraw(q2, owner)).toBe(true);
    expect(canWithdraw(entries[0], owner)).toBe(false);
  });
  it("edits the last queued message this person may withdraw on ↑", () => {
    expect(lastQueued(entries, owner)?.id).toBe("q2");
    expect(lastQueued(entries, alice)?.id).toBe("q1");
    expect(lastQueued(entries, { principalID: "bob" })).toBeUndefined();
    expect(lastQueued(entries.slice(0, 2), owner)).toBeUndefined();
  });
  it("hints at the queue in the composer", () => {
    expect(queueHint(chat("idle", entries.slice(0, 2)), owner)).toBe("");
    expect(queueHint(chat("running"), owner)).toBe(
      "2 messages queued · will send in order after this turn · ↑ edits the last one",
    );
    expect(queueHint(chat("running"), { principalID: "bob" })).toBe(
      "2 messages queued · will send in order after this turn",
    );
    expect(queueHint(chat("interrupted"), owner)).toMatch(/^2 messages held/);
    expect(
      queueHint(chat("running", [entries[0], entries[5]]), owner),
    ).toMatch(/^1 message queued/);
  });
});

describe("edit and resend", () => {
  it("edits a sent message only while a rewind is possible", () => {
    const sent = entries[0];
    expect(canEditAndResend(sent, chat("idle"), true)).toBe(true);
    expect(canEditAndResend(sent, chat("interrupted"), true)).toBe(true);
    expect(canEditAndResend(sent, chat("running"), true)).toBe(false);
    expect(canEditAndResend(sent, chat("idle"), false)).toBe(false);
    expect(
      canEditAndResend(sent, { ...chat("idle"), archived: true }, true),
    ).toBe(false);
    expect(canEditAndResend(entries[5], chat("idle"), true)).toBe(false);
    expect(canEditAndResend(entries[3], chat("idle"), true)).toBe(false);
  });
  it("rewinds the conversation, and the code when asked", () => {
    expect(editingScope({ code: false })).toBe("conversation");
    expect(editingScope({ code: true })).toBe("both");
  });
  it("says how many queued messages a rewind withdrew", () => {
    expect(
      rewindOutcome({
        messageID: "u1",
        what: "conversation",
        conversation: "rewound",
        withdrawn: 2,
      }),
    ).toBe("2 queued messages withdrawn");
    expect(
      rewindOutcome({
        messageID: "u1",
        what: "both",
        conversation: "pending",
        withdrawn: 1,
        restored: ["a.txt"],
      }),
    ).toBe(
      "1 file restored, 0 files removed; the agent forgets the messages when its session resumes; 1 queued message withdrawn",
    );
  });
});
