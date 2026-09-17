import { describe, it, expect } from "vitest";
import {
  fenceDiff,
  hasDiff,
  isDiffFence,
  looseDiff,
  parseDiff,
  type DiffFile,
} from "./diff";

const gitDiff = `diff --git a/src/a.ts b/src/a.ts
index 1234567..89abcde 100644
--- a/src/a.ts
+++ b/src/a.ts
@@ -1,3 +1,4 @@ export function a() {
 const x = 1;
-const y = 2;
+const y = 3;
+const z = 4;
 return x + y;
`;

function diffs(segments: ReturnType<typeof parseDiff>): DiffFile[] {
  return segments.flatMap((s) => (s.kind === "diff" ? [s.file] : []));
}

describe("unified diff parsing", () => {
  it("names diff and patch fences", () => {
    expect(isDiffFence("diff")).toBe(true);
    expect(isDiffFence("patch")).toBe(true);
    expect(isDiffFence("ts")).toBe(false);
  });
  it("reads a git diff into a file, its hunk and numbered lines", () => {
    const segments = parseDiff(gitDiff);
    expect(segments).toHaveLength(1);
    const [file] = diffs(segments);
    expect(file.path).toBe("src/a.ts");
    expect(file.status).toBe("");
    expect(file.additions).toBe(2);
    expect(file.deletions).toBe(1);
    expect(file.hunks).toHaveLength(1);
    expect(file.hunks[0].header).toBe("@@ -1,3 +1,4 @@ export function a() {");
    expect(file.hunks[0].lines).toEqual([
      { kind: "ctx", text: "const x = 1;", oldNo: 1, newNo: 1 },
      { kind: "del", text: "const y = 2;", oldNo: 2 },
      { kind: "add", text: "const y = 3;", newNo: 2 },
      { kind: "add", text: "const z = 4;", newNo: 3 },
      { kind: "ctx", text: "return x + y;", oldNo: 3, newNo: 4 },
    ]);
    expect(hasDiff(segments)).toBe(true);
  });
  it("keeps prose around the diff as text segments", () => {
    const text = `completed\n${gitDiff}\nDone.`;
    const segments = parseDiff(text);
    expect(segments.map((s) => s.kind)).toEqual(["text", "diff", "text"]);
    expect(segments[0]).toEqual({ kind: "text", text: "completed" });
    expect(segments[2]).toEqual({ kind: "text", text: "\nDone." });
  });
  it("reads Codex's fileChange detail: a path line then bare hunks", () => {
    const text =
      "README.md\n@@ -1 +1,2 @@\n-# Old\n+# New\n+intro\n\\ No newline at end of file\n";
    const segments = parseDiff(text);
    expect(segments[0]).toEqual({ kind: "text", text: "README.md" });
    const [file] = diffs(segments);
    expect(file.path).toBe("");
    expect(file.hunks[0].lines.map((l) => l.kind)).toEqual([
      "del",
      "add",
      "add",
      "meta",
    ]);
    expect(file.hunks[0].lines[3].text).toBe("No newline at end of file");
    expect(hasDiff(segments)).toBe(true);
  });
  it("splits several files and reads the git status lines", () => {
    const text = [
      "diff --git a/new.txt b/new.txt",
      "new file mode 100644",
      "--- /dev/null",
      "+++ b/new.txt",
      "@@ -0,0 +1 @@",
      "+hello",
      "diff --git a/gone.txt b/gone.txt",
      "deleted file mode 100644",
      "--- a/gone.txt",
      "+++ /dev/null",
      "@@ -1 +0,0 @@",
      "-bye",
      "diff --git a/pic.png b/pic.png",
      "Binary files a/pic.png and b/pic.png differ",
    ].join("\n");
    const files = diffs(parseDiff(text));
    expect(files.map((f) => [f.path, f.status])).toEqual([
      ["new.txt", "new file"],
      ["gone.txt", "deleted"],
      ["pic.png", "binary"],
    ]);
    expect(files[2].hunks).toHaveLength(0);
  });
  it("uses the counts only to settle ambiguous lines", () => {
    // A deleted `-- x` line looks like a file header until the count says
    // the hunk still has old lines; after the count is spent it ends the
    // hunk. A stripped blank context line is still context while lines
    // remain, and ends the hunk after.
    const text =
      "@@ -1,2 +1,1 @@\n--- x\n\n--- b/next\n+++ b/next\n@@ -1 +1 @@\n-a\n+b";
    const files = diffs(parseDiff(text));
    expect(files).toHaveLength(2);
    expect(files[0].hunks[0].lines).toEqual([
      { kind: "del", text: "-- x", oldNo: 1 },
      { kind: "ctx", text: "", oldNo: 2, newNo: 1 },
    ]);
    expect(files[1].path).toBe("next");
  });
  it("tolerates counts a hand-written diff got wrong", () => {
    const text = "@@ -1,1 +1,1 @@\n-a\n+b\n+c\n+d";
    const [file] = diffs(parseDiff(text));
    expect(file.additions).toBe(3);
    expect(file.hunks[0].lines.map((l) => l.newNo)).toEqual([
      undefined,
      1,
      2,
      3,
    ]);
  });
  it("leaves markdown rules, the word diff and stray pluses as prose", () => {
    const text = "---\n+++\nsome +1 remark\n- a list\ndiff the two\n";
    const segments = parseDiff(text);
    expect(segments).toEqual([{ kind: "text", text: text.trimEnd() }]);
    expect(hasDiff(segments)).toBe(false);
    // A `---`/`+++` pair without a hunk is not a diff either.
    expect(hasDiff(parseDiff("--- a\n+++ b\nno hunk"))).toBe(false);
  });
  it("strips CRLF and diff -u timestamps", () => {
    const text =
      "--- a.txt\t2026-09-17 10:00:00\r\n+++ b.txt\t2026-09-17 10:01:00\r\n@@ -1 +1 @@\r\n-x\r\n+y\r\n";
    const [file] = diffs(parseDiff(text));
    expect(file.path).toBe("b.txt");
    expect(file.hunks[0].lines.map((l) => l.text)).toEqual(["x", "y"]);
  });
  it("colours a headerless fence by prefix, without numbers", () => {
    const file = looseDiff("- old\n+ new\n context\nplain\n@@ note\n");
    expect(file.hunks).toHaveLength(1);
    expect(file.hunks[0].header).toBe("");
    expect(file.hunks[0].lines).toEqual([
      { kind: "del", text: " old" },
      { kind: "add", text: " new" },
      { kind: "ctx", text: "context" },
      { kind: "ctx", text: "plain" },
      { kind: "meta", text: "@@ note" },
    ]);
    expect(file.additions).toBe(1);
    expect(file.deletions).toBe(1);
    // The fence uses the parsed form when there are hunks, else this one.
    expect(diffs(fenceDiff(gitDiff))[0].path).toBe("src/a.ts");
    expect(diffs(fenceDiff("-a\n+b"))[0].hunks[0].header).toBe("");
  });
});
