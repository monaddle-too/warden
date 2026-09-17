// Pure logic behind DiffView: finding unified diffs in text the agent wrote
// (a ```diff fence, a tool's output, Codex's fileChange detail) and splitting
// them into files, hunks and lines. Everything here is line-oriented string
// work; nothing is interpreted, so the view only ever renders text nodes.

export type DiffLine = {
  kind: "add" | "del" | "ctx" | "meta";
  /* The line without its +/-/space prefix. */
  text: string;
  oldNo?: number;
  newNo?: number;
};

export type DiffHunk = {
  /* The `@@ … @@` line, empty for a fence without hunk headers. */
  header: string;
  lines: DiffLine[];
};

export type DiffFile = {
  /* What the header names: the new path, or the old one for a deletion;
     empty when the hunks had no `---`/`+++` or `diff` line before them. */
  path: string;
  /* new file, deleted, renamed, binary … from the git header lines. */
  status: string;
  hunks: DiffHunk[];
  additions: number;
  deletions: number;
};

/* A detail string is a sequence of prose and diffs; the view keeps the prose
   (Codex's path line, a command's status line) as plain text between them. */
export type DiffSegment =
  | { kind: "text"; text: string }
  | { kind: "diff"; file: DiffFile };

/* Fences that hold a diff rather than code. */
export function isDiffFence(language: string): boolean {
  return language === "diff" || language === "patch";
}

const HUNK = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/;
/* `diff --git a/x b/y`, `diff -u x y`: the word alone is prose. */
const DIFF_LINE = /^diff -/;
const GIT_HEADER =
  /^(index |old mode |new mode |deleted file mode |new file mode |similarity index |dissimilarity index |rename (from|to) |copy (from|to) |Binary files )/;

/* `--- a/x` → `x`; `a/` and `b/` are git's prefixes and a tab starts the
   timestamp `diff -u` appends. */
function headerPath(line: string): string {
  const path = line.slice(4).split("\t")[0].trim();
  if (path === "/dev/null") return "";
  return path.replace(/^[ab]\//, "");
}

function statusOf(head: string[]): string {
  for (const line of head) {
    if (line.startsWith("new file mode")) return "new file";
    if (line.startsWith("deleted file mode")) return "deleted";
    if (line.startsWith("rename from")) return "renamed";
    if (line.startsWith("Binary files")) return "binary";
  }
  return "";
}

/* Where a diff can begin at line `i`: a git `diff` line, a `---`/`+++` pair
   or a bare hunk. `---` alone is also a markdown rule, so the pair is
   required, and it must lead to a hunk unless a `diff` line introduced it. */
function fileStart(lines: string[], i: number): boolean {
  const line = lines[i];
  if (DIFF_LINE.test(line) || HUNK.test(line)) return true;
  if (line.startsWith("--- ") && lines[i + 1]?.startsWith("+++ ")) {
    let j = i + 2;
    while (j < lines.length && GIT_HEADER.test(lines[j])) j++;
    return j < lines.length && HUNK.test(lines[j]);
  }
  return false;
}

/* Reads one hunk starting at the `@@` line. Hand-written diffs often carry
   wrong counts, so the counts decide only the ambiguous cases (an empty
   line that a tool stripped, a `---` that is a deleted `--`) and the prefix
   decides the rest. Returns the next line index. */
function readHunk(lines: string[], i: number, file: DiffFile): number {
  const m = HUNK.exec(lines[i])!;
  let oldNo = Number(m[1]),
    newNo = Number(m[3]),
    oldLeft = m[2] === undefined ? 1 : Number(m[2]),
    newLeft = m[4] === undefined ? 1 : Number(m[4]);
  const hunk: DiffHunk = { header: lines[i], lines: [] };
  file.hunks.push(hunk);
  i++;
  for (; i < lines.length; i++) {
    const line = lines[i];
    const c = line[0];
    if (c === "\\") {
      hunk.lines.push({ kind: "meta", text: line.slice(1).trim() });
    } else if (c === "+") {
      if (newLeft <= 0 && line.startsWith("+++ ")) break;
      hunk.lines.push({ kind: "add", text: line.slice(1), newNo: newNo++ });
      newLeft--;
      file.additions++;
    } else if (c === "-") {
      if (oldLeft <= 0 && line.startsWith("--- ")) break;
      hunk.lines.push({ kind: "del", text: line.slice(1), oldNo: oldNo++ });
      oldLeft--;
      file.deletions++;
    } else if (c === " " || (line === "" && oldLeft > 0 && newLeft > 0)) {
      hunk.lines.push({
        kind: "ctx",
        text: line.slice(1),
        oldNo: oldNo++,
        newNo: newNo++,
      });
      oldLeft--;
      newLeft--;
    } else {
      break;
    }
  }
  return i;
}

/* Reads one file's header and hunks starting at a `fileStart` line. */
function readFile(lines: string[], i: number): [DiffFile, number] {
  const file: DiffFile = {
    path: "",
    status: "",
    hunks: [],
    additions: 0,
    deletions: 0,
  };
  const head: string[] = [];
  if (DIFF_LINE.test(lines[i])) {
    const git = /^diff --git a\/(.*) b\/(.*)$/.exec(lines[i]);
    file.path = git ? git[2] : "";
    i++;
    while (i < lines.length && GIT_HEADER.test(lines[i])) head.push(lines[i++]);
  }
  if (lines[i]?.startsWith("--- ") && lines[i + 1]?.startsWith("+++ ")) {
    file.path = headerPath(lines[i + 1]) || headerPath(lines[i]) || file.path;
    i += 2;
    while (i < lines.length && GIT_HEADER.test(lines[i])) head.push(lines[i++]);
  }
  file.status = statusOf(head);
  while (i < lines.length && HUNK.test(lines[i])) i = readHunk(lines, i, file);
  return [file, i];
}

/* Splits text into prose and the unified diffs found in it. A trailing
   newline does not become an empty prose segment. */
export function parseDiff(text: string): DiffSegment[] {
  const lines = text.split(/\r?\n/);
  if (lines[lines.length - 1] === "") lines.pop();
  const segments: DiffSegment[] = [];
  let prose: string[] = [];
  const flush = () => {
    if (prose.length) segments.push({ kind: "text", text: prose.join("\n") });
    prose = [];
  };
  for (let i = 0; i < lines.length; ) {
    if (fileStart(lines, i)) {
      const [file, next] = readFile(lines, i);
      if (next > i) {
        flush();
        segments.push({ kind: "diff", file });
        i = next;
        continue;
      }
    }
    prose.push(lines[i++]);
  }
  flush();
  return segments;
}

/* True when the text holds at least one real hunk, which is what makes an
   activity detail worth showing as a diff instead of raw output. */
export function hasDiff(segments: DiffSegment[]): boolean {
  return segments.some((s) => s.kind === "diff" && s.file.hunks.length > 0);
}

/* A ```diff fence without hunk headers (just +/- lines, as people write
   them by hand) is still coloured by prefix; there are no line numbers to
   compute, so it is one headerless hunk. */
export function looseDiff(text: string): DiffFile {
  const lines = text.split(/\r?\n/);
  if (lines[lines.length - 1] === "") lines.pop();
  const file: DiffFile = {
    path: "",
    status: "",
    hunks: [{ header: "", lines: [] }],
    additions: 0,
    deletions: 0,
  };
  for (const line of lines) {
    const c = line[0];
    if (c === "+") {
      file.hunks[0].lines.push({ kind: "add", text: line.slice(1) });
      file.additions++;
    } else if (c === "-") {
      file.hunks[0].lines.push({ kind: "del", text: line.slice(1) });
      file.deletions++;
    } else if (c === "@" || c === "\\") {
      file.hunks[0].lines.push({ kind: "meta", text: line });
    } else {
      file.hunks[0].lines.push({
        kind: "ctx",
        text: c === " " ? line.slice(1) : line,
      });
    }
  }
  return file;
}

/* What a ```diff fence shows: the parsed diff when it has hunks, else the
   prefix-coloured fallback. */
export function fenceDiff(text: string): DiffSegment[] {
  const segments = parseDiff(text);
  return hasDiff(segments)
    ? segments
    : [{ kind: "diff", file: looseDiff(text) }];
}
