import { describe, expect, it } from "vitest";
import {
  FOLD_LINES,
  diffCounts,
  editSegments,
  fetchHost,
  foldText,
  formatElapsed,
  hitCount,
  hostResult,
  hostStatus,
  inputText,
  lineCount,
  readCount,
  shortPath,
  subagentInput,
  subagentProgress,
  taskElapsed,
  todoItems,
  todoProgress,
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
  it("titles a host call by what it did and reads its result", () => {
    const host = (name: string, input: Record<string, unknown>) =>
      entry(
        {
          kind: "mcp",
          name,
          server: "warden",
          status: "completed",
          target: "host",
          input,
        },
        { text: "warden · " + name },
      );
    expect(toolTitle(host("host_run", { command: "uname -a" }))).toBe(
      "uname -a",
    );
    expect(toolTitle(host("host_put", { from: "/a", to: "/b" }))).toBe(
      "/a → /b",
    );
    expect(toolTitle(host("host_expose", { port: 18830 }))).toBe(
      "Expose host port 18830",
    );
    expect(toolTitle(host("host_status", {}))).toBe("Host status");
    expect(
      hostResult('{"exitCode":3,"timedOut":false,"output":"hi\\n"}'),
    ).toEqual({ exitCode: 3, timedOut: false, output: "hi\n" });
    expect(hostStatus(hostResult('{"exitCode":3,"output":""}'))).toBe("exit 3");
    expect(hostStatus(hostResult('{"exitCode":-1,"timedOut":true}'))).toBe(
      "timed out",
    );
    expect(hostStatus(hostResult('{"exitCode":0,"output":"ok"}'))).toBe("");
    expect(hostResult("host access is off for this workspace")).toEqual({
      text: "host access is off for this workspace",
    });
    expect(hostResult('{"url":"http://x/","port":1}')).toEqual({
      url: "http://x/",
    });
    expect(hostResult("")).toEqual({});
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

describe("subagent and todo cards", () => {
  it("counts a subagent's tool calls and whether one still runs", () => {
    const children = [
      entry({ kind: "command", status: "completed" }, { id: "c1" }),
      entry({ kind: "read", status: "running" }, { id: "c2" }),
      entry(undefined, { id: "m", role: "assistant" }),
    ];
    expect(subagentProgress(children)).toEqual({
      steps: 2,
      running: true,
      step: "",
    });
    expect(subagentProgress([children[0]])).toEqual({
      steps: 1,
      running: false,
      step: "",
    });
    // The agent's own account (task_progress) when it runs ahead of the
    // entries, and what it says the subagent is doing: its words, else
    // the tool it used last.
    expect(
      subagentProgress(children, { toolCalls: 5, lastTool: "Grep" }),
    ).toEqual({ steps: 5, running: true, step: "using Grep" });
    expect(
      subagentProgress(children, {
        toolCalls: 1,
        lastTool: "Read",
        activity: "Reading hello.txt",
      }),
    ).toEqual({ steps: 2, running: true, step: "Reading hello.txt" });
    expect(subagentProgress(children, { toolCalls: 1 })).toEqual({
      steps: 2,
      running: true,
      step: "",
    });
  });
  it("measures a subagent's time to its end, or to now while it runs", () => {
    const done = entry(
      { kind: "task", status: "completed" },
      { createdAt: 100, endedAt: 163 },
    );
    expect(taskElapsed(done, 999)).toBe(63);
    const running = entry(
      { kind: "task", status: "running" },
      { createdAt: 100 },
    );
    expect(taskElapsed(running, 130)).toBe(30);
    expect(taskElapsed(entry({ kind: "task", status: "completed" }), 5)).toBe(
      0,
    );
    expect(formatElapsed(4.4)).toBe("4s");
    expect(formatElapsed(72)).toBe("1m 12s");
    expect(formatElapsed(7500)).toBe("2h 5m");
  });
  it("reads the subagent's type and prompt from the call's input", () => {
    expect(
      subagentInput({
        kind: "task",
        status: "running",
        input: { subagent_type: "Explore", prompt: "Find the tests." },
      }),
    ).toEqual({ type: "Explore", prompt: "Find the tests." });
    expect(subagentInput(undefined)).toEqual({ type: "", prompt: "" });
  });
  it("reads a todo list and its progress, tolerating bad items", () => {
    const items = todoItems({
      kind: "todo",
      status: "completed",
      input: {
        todos: [
          { content: "Parse", status: "completed" },
          { content: "Test", status: "in_progress", activeForm: "Testing" },
          { content: "Ship", status: "weird" },
          { status: "pending" },
          null,
        ],
      },
    });
    expect(items).toEqual([
      { content: "Parse", status: "completed", activeForm: "" },
      { content: "Test", status: "in_progress", activeForm: "Testing" },
      { content: "Ship", status: "pending", activeForm: "" },
    ]);
    expect(todoProgress(items)).toBe("1 of 3 done");
    expect(todoItems({ kind: "todo", status: "completed" })).toEqual([]);
  });
});

describe("rich reads", () => {
  it("counts an image's pixels, a PDF's pages and a notebook's cells", () => {
    expect(readCount({ kind: "image", width: 64, height: 48 })).toBe("64×48");
    expect(readCount({ kind: "image" })).toBe("image");
    expect(readCount({ kind: "pdf", pages: 2 })).toBe("2 pages");
    expect(readCount({ kind: "pdf", pages: 1 })).toBe("1 page");
    expect(readCount({ kind: "pdf" })).toBe("PDF");
    expect(
      readCount({
        kind: "notebook",
        cells: [
          { type: "code", language: "python", text: "print(1)" },
          { type: "markdown", text: "# T" },
        ],
      }),
    ).toBe("2 cells");
    expect(readCount({ kind: "notebook" })).toBe("0 cells");
  });
});
