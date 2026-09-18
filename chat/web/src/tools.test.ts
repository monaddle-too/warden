import { describe, expect, it } from "vitest";
import {
  FOLD_LINES,
  diffCounts,
  editSegments,
  fetchHost,
  foldText,
  hitCount,
  inputText,
  lineCount,
  shortPath,
  toolFailed,
  toolKind,
  toolRunning,
  toolTitle,
} from "./tools";
import type { Entry, Tool } from "./types";

const entry = (tool: Tool | undefined, extra: Partial<Entry> = {}): Entry => ({
  id: "e",
  role: "activity",
  text: "",
  detail: "",
  createdAt: 0,
  isStreaming: false,
  delivery: "",
  tool,
  ...extra,
});

describe("tool cards", () => {
  it("reads the kind and state from the recorded tool", () => {
    expect(toolKind(entry(undefined))).toBeUndefined();
    const running = entry({ kind: "command", status: "running" });
    expect(toolKind(running)).toBe("command");
    expect(toolRunning(running)).toBe(true);
    expect(toolRunning(entry({ kind: "read", status: "completed" }))).toBe(
      false,
    );
    expect(
      toolRunning(
        entry({ kind: "read", status: "completed" }, { isStreaming: true }),
      ),
    ).toBe(true);
    expect(toolFailed({ kind: "command", status: "failed" })).toBe(true);
    expect(toolFailed({ kind: "command", status: "declined" })).toBe(true);
    expect(toolFailed({ kind: "command", status: "completed" })).toBe(false);
    expect(toolFailed({ kind: "command", status: "running" })).toBe(false);
    expect(toolFailed(undefined)).toBe(false);
  });
  it("titles a card by its text, else its tool", () => {
    expect(
      toolTitle(
        entry({ kind: "read", status: "completed" }, { text: "Read a.go" }),
      ),
    ).toBe("Read a.go");
    expect(
      toolTitle(entry({ kind: "other", name: "Monitor", status: "completed" })),
    ).toBe("Monitor");
    expect(toolTitle(entry(undefined))).toBe("Agent activity");
  });
  it("shows workspace paths relative to the workspace", () => {
    expect(shortPath("/home/agent/workspace/chat/main.go")).toBe(
      "chat/main.go",
    );
    expect(shortPath("/home/agent/workspace")).toBe(".");
    expect(shortPath("/etc/hosts")).toBe("/etc/hosts");
    expect(shortPath("chat/main.go")).toBe("chat/main.go");
  });
  it("folds long output to its first lines, or its last while running", () => {
    const lines = Array.from({ length: 30 }, (_, i) => `line ${i + 1}`);
    const text = lines.join("\n") + "\n";
    const fold = foldText(text);
    expect(fold.total).toBe(30);
    expect(fold.hidden).toBe(30 - FOLD_LINES);
    expect(fold.shown.split("\n")).toEqual(lines.slice(0, FOLD_LINES));
    const tail = foldText(text, FOLD_LINES, true);
    expect(tail.shown.split("\n")).toEqual(lines.slice(30 - FOLD_LINES));
    expect(foldText("a\nb\n")).toEqual({ shown: "a\nb", hidden: 0, total: 2 });
    expect(foldText("")).toEqual({ shown: "", hidden: 0, total: 0 });
    expect(foldText("x", 3)).toEqual({ shown: "x", hidden: 0, total: 1 });
  });
  it("counts what a search or read returned", () => {
    expect(hitCount("Found 3 files\na\nb\nc\n")).toEqual({
      count: 3,
      unit: "files",
    });
    expect(hitCount("Found 1 file\na\n")).toEqual({ count: 1, unit: "file" });
    expect(hitCount("No files found")).toEqual({ count: 0, unit: "results" });
    expect(hitCount("No matches found")).toEqual({ count: 0, unit: "results" });
    expect(hitCount("a.go:1:x\nb.go:4:x\n\n")).toEqual({
      count: 2,
      unit: "results",
    });
    expect(hitCount("")).toEqual({ count: 0, unit: "results" });
    expect(lineCount("1\ta\n2\tb\n")).toBe(2);
    expect(lineCount("")).toBe(0);
  });
  it("splits a file change's detail into its diffs", () => {
    const numbered =
      "notes.txt\ndiff --git a/notes.txt b/notes.txt\n--- a/notes.txt\n+++ b/notes.txt\n@@ -1,4 +1,4 @@\n alpha\n beta\n-gamma\n+GAMMA\n delta\n\n";
    const segments = editSegments(numbered);
    expect(segments).toHaveLength(1);
    const file = segments[0].kind === "diff" ? segments[0].file : undefined;
    expect(file?.path).toBe("notes.txt");
    expect(file?.hunks[0].header).toBe("@@ -1,4 +1,4 @@");
    expect(file?.hunks[0].lines.map((l) => l.kind)).toEqual([
      "ctx",
      "ctx",
      "del",
      "add",
      "ctx",
    ]);
    expect(file?.hunks[0].lines[3].newNo).toBe(3);
    expect(diffCounts(segments)).toEqual({ additions: 1, deletions: 1 });
  });
  it("renders a headerless hunk by prefix, without line numbers", () => {
    const loose =
      "a.go\ndiff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n-return 1\n+return 2\n+// more\n\n";
    const [segment] = editSegments(loose);
    expect(segment.kind).toBe("diff");
    if (segment.kind !== "diff") return;
    expect(segment.file.path).toBe("a.go");
    expect(segment.file.hunks[0].header).toBe("");
    expect(
      segment.file.hunks[0].lines.map((l) => `${l.kind}:${l.text}`),
    ).toEqual(["del:return 1", "add:return 2", "add:// more"]);
    expect(segment.file.hunks[0].lines[0].oldNo).toBeUndefined();
    expect(diffCounts([segment])).toEqual({ additions: 2, deletions: 1 });
  });
  it("marks a written file as new and keeps several files apart", () => {
    const two =
      "hello.txt\ndiff --git a/hello.txt b/hello.txt\nnew file mode 100644\n--- /dev/null\n+++ b/hello.txt\n@@ -0,0 +1,2 @@\n+hello\n+world\n\nnb.ipynb\ndiff --git a/nb.ipynb b/nb.ipynb\n--- a/nb.ipynb\n+++ b/nb.ipynb\n@@ cell c3\n+print(1)\n\n";
    const segments = editSegments(two);
    expect(segments.map((s) => (s.kind === "diff" ? s.file.path : ""))).toEqual(
      ["hello.txt", "nb.ipynb"],
    );
    const [created, cell] = segments.map((s) =>
      s.kind === "diff" ? s.file : undefined,
    );
    expect(created?.status).toBe("new file");
    expect(created?.additions).toBe(2);
    expect(cell?.hunks[0].header).toBe("");
    expect(cell?.hunks[0].lines.map((l) => l.kind)).toEqual(["meta", "add"]);
    expect(diffCounts(segments)).toEqual({ additions: 3, deletions: 0 });
    // A change whose diff is empty (a write of an empty file) is no segment.
    expect(editSegments("x\ndiff --git a/x b/x\n--- a/x\n+++ b/x\n\n")).toEqual(
      [],
    );
    expect(editSegments("")).toEqual([]);
  });
  it("names a fetch by its host and shows an input as ordered JSON", () => {
    expect(fetchHost("https://example.com:8443/a?b")).toBe("example.com:8443");
    expect(fetchHost("not a url")).toBe("not a url");
    expect(inputText({ z: 1, a: "x" })).toBe('{\n  "a": "x",\n  "z": 1\n}');
    expect(inputText({})).toBe("");
    expect(inputText(undefined)).toBe("");
  });
});
