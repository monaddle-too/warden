import { describe, it, expect } from "vitest";
import {
  canRewind,
  canUndoRewind,
  changesSummary,
  doubleEscape,
  excerpt,
  rewindOutcome,
  rewindTargets,
  splitDiff,
  undoHint,
  undoOffersCode,
  undoOutcome,
  whatAllowed,
} from "./rewind";
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

describe("rewind targets", () => {
  it("lists the conversation's own user messages, numbered, with their checkpoints", () => {
    const entries = [
      entry({ id: "u1", role: "user", text: "first" }),
      entry({ id: "a1", role: "assistant", text: "ok" }),
      entry({ id: "t1", role: "activity", text: "Agent" }),
      entry({
        id: "p1",
        role: "user",
        text: "subagent prompt",
        parentID: "t1",
      }),
      entry({ id: "u2", role: "user", text: "second" }),
      entry({ id: "r1", role: "rewind", text: "Rewound…" }),
    ];
    const targets = rewindTargets(entries, ["u2"]);
    expect(targets.map((t) => [t.entry.id, t.number, t.checkpoint])).toEqual([
      ["u1", 1, false],
      ["u2", 2, true],
    ]);
    expect(whatAllowed("code", targets[0])).toBe(false);
    expect(whatAllowed("conversation", targets[0])).toBe(true);
    expect(whatAllowed("both", targets[1])).toBe(true);
  });

  it("allows a rewind only while the chat is idle and not archived", () => {
    expect(canRewind({ status: "idle", archived: false })).toBe(true);
    expect(canRewind({ status: "interrupted", archived: false })).toBe(true);
    expect(canRewind({ status: "failed", archived: false })).toBe(true);
    for (const status of ["running", "queued", "stopping"])
      expect(canRewind({ status, archived: false })).toBe(false);
    expect(canRewind({ status: "idle", archived: true })).toBe(false);
  });

  it("shortens a message for a list", () => {
    expect(excerpt("  Make   it\nblue  ")).toBe("Make it blue");
    expect(excerpt("")).toBe("(attachments)");
    expect(excerpt("x".repeat(100), 10)).toBe("xxxxxxxxxx…");
  });

  it("recognises Esc pressed twice", () => {
    expect(doubleEscape(0, 1000)).toBe(false);
    expect(doubleEscape(1000, 1400)).toBe(true);
    expect(doubleEscape(1000, 1700)).toBe(false);
  });

  it("describes what a rewind did", () => {
    expect(
      rewindOutcome({
        messageID: "m",
        what: "code",
        restored: ["a"],
        removed: [],
      }),
    ).toBe("1 file restored, 0 files removed");
    expect(rewindOutcome({ messageID: "m", what: "code" })).toBe(
      "the workspace already matched the checkpoint",
    );
    expect(
      rewindOutcome({
        messageID: "m",
        what: "conversation",
        conversation: "rewound",
      }),
    ).toBe("");
    expect(
      rewindOutcome({
        messageID: "m",
        what: "both",
        restored: ["a", "b"],
        removed: ["c"],
        conversation: "pending",
      }),
    ).toBe(
      "2 files restored, 1 file removed; the agent forgets the messages when its session resumes",
    );
    expect(
      rewindOutcome({
        messageID: "m",
        what: "conversation",
        conversation: "fresh",
      }),
    ).toContain("could not rewind");
  });
});

describe("session changes", () => {
  const diff = `diff --git a/src/a.ts b/src/a.ts
--- a/src/a.ts
+++ b/src/a.ts
@@ -1 +1 @@
-const y = 2;
+const y = 3;
diff --git a/new.txt b/new.txt
new file mode 100644
--- /dev/null
+++ b/new.txt
@@ -0,0 +1 @@
+hello
`;
  it("splits one git diff into its files by path", () => {
    const files = splitDiff(diff);
    expect([...files.keys()]).toEqual(["src/a.ts", "new.txt"]);
    expect(files.get("new.txt")).toContain("+hello");
    expect(files.get("src/a.ts")).not.toContain("hello");
    expect(splitDiff("").size).toBe(0);
  });

  it("sums the counts", () => {
    expect(
      changesSummary([
        { path: "a", added: 3, removed: 1 },
        { path: "b", added: 0, removed: 0, binary: true },
      ]),
    ).toEqual({ files: 2, added: 3, removed: 1 });
  });
});

describe("undoing a rewind", () => {
  const marker = entry({
    id: "m1",
    role: "rewind",
    text: "Rewound to before “x” (code and conversation)",
    rewind: {
      messageID: "u2",
      what: "both",
      conversation: "rewound",
      before: "m1",
    },
  });
  it("offers Undo on the marker the chat still keeps the tail for, while idle", () => {
    expect(
      canUndoRewind(marker, {
        status: "idle",
        archived: false,
        undoRewind: "m1",
      }),
    ).toBe(true);
    expect(
      canUndoRewind(marker, {
        status: "interrupted",
        archived: false,
        undoRewind: "m1",
      }),
    ).toBe(true);
    expect(
      canUndoRewind(marker, {
        status: "idle",
        archived: false,
        undoRewind: "m2",
      }),
    ).toBe(false);
    expect(canUndoRewind(marker, { status: "idle", archived: false })).toBe(
      false,
    );
    expect(
      canUndoRewind(marker, {
        status: "running",
        archived: false,
        undoRewind: "m1",
      }),
    ).toBe(false);
    expect(
      canUndoRewind(marker, {
        status: "idle",
        archived: true,
        undoRewind: "m1",
      }),
    ).toBe(false);
    expect(
      canUndoRewind(entry({ id: "m1", role: "system" }), {
        status: "idle",
        archived: false,
        undoRewind: "m1",
      }),
    ).toBe(false);
  });
  it("offers the files back only for a both-rewind that recorded them", () => {
    expect(undoOffersCode(marker)).toBe(true);
    expect(undoOffersCode({ rewind: { messageID: "u2", what: "both" } })).toBe(
      false,
    );
    expect(
      undoOffersCode({
        rewind: { messageID: "u2", what: "conversation", before: "m1" },
      }),
    ).toBe(false);
    expect(undoOffersCode({})).toBe(false);
    expect(undoHint(marker)).toBe(
      "Undo puts the removed messages back (until the next turn); the files can come back too",
    );
    expect(undoHint({ rewind: { messageID: "u2", what: "both" } })).toBe(
      "Undo puts the removed messages back (until the next turn); the workspace stays as it is",
    );
    expect(
      undoHint({ rewind: { messageID: "u2", what: "conversation" } }),
    ).toBe("Undo puts the removed messages back (until the next turn)");
  });
  it("says what an undo did", () => {
    expect(
      undoOutcome({
        messageID: "u2",
        what: "conversation",
        entries: 1,
        session: "cancelled",
      }),
    ).toBe("1 entry restored; the agent's session never saw the rewind");
    expect(
      undoOutcome({
        messageID: "u2",
        what: "conversation",
        entries: 3,
        requeued: 1,
        session: "resumed",
      }),
    ).toBe(
      "3 entries restored; 1 message queued again and held; the agent's session continues where it was",
    );
    expect(
      undoOutcome({
        messageID: "u2",
        what: "both",
        entries: 2,
        session: "fresh",
        code: "restored",
        restored: ["a.txt", "b.txt"],
        removed: [],
      }),
    ).toBe(
      "2 entries restored; the agent's session cannot take the messages back; the next message starts a new one with the conversation so far as context; the workspace is back as it was before the rewind (2 files restored, 0 files removed)",
    );
    expect(
      undoOutcome({
        messageID: "u2",
        what: "both",
        entries: 2,
        session: "",
        code: "kept",
      }),
    ).toBe("2 entries restored; the workspace stays as the rewind left it");
  });
});
