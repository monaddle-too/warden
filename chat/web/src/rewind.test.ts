import { describe, it, expect } from "vitest";
import {
  canRewind,
  changesSummary,
  doubleEscape,
  excerpt,
  rewindOutcome,
  rewindTargets,
  splitDiff,
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
