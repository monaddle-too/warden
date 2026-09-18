import type { MemoryFile } from "./types";

/* The workspace's memory files as the panel shows them (parity item 13):
   grouped by what they are, labelled, and the path rules for a new file
   mirrored from the runner's (chat/internal/sandbox/memory.go), so a bad
   name is refused before the round trip; the service validates again. */

/* The fixed instruction files at the workspace root, in the order the
   panel lists them. */
export const WORKSPACE_FILES = [
  "CLAUDE.md",
  "CLAUDE.local.md",
  "AGENTS.md",
  ".claude/CLAUDE.md",
];

export type MemoryGroup = {
  kind: "instructions" | "rules" | "auto";
  title: string;
  files: MemoryFile[];
};

/* Files by group: the root instruction files, the rules under
   .claude/rules, the auto-memory files; empty groups left out. Within a
   group the listing's order (the runner sorts by name) is kept, except the
   root files, which come in WORKSPACE_FILES order. */
export function groupMemory(files: MemoryFile[]): MemoryGroup[] {
  const instructions = files
    .filter((f) => f.scope === "workspace" && WORKSPACE_FILES.includes(f.path))
    .sort(
      (a, b) =>
        WORKSPACE_FILES.indexOf(a.path) - WORKSPACE_FILES.indexOf(b.path),
    );
  const rules = files.filter(
    (f) => f.scope === "workspace" && f.path.startsWith(".claude/rules/"),
  );
  const auto = files.filter((f) => f.scope === "auto");
  const groups: MemoryGroup[] = [];
  if (instructions.length)
    groups.push({
      kind: "instructions",
      title: "Instructions",
      files: instructions,
    });
  if (rules.length)
    groups.push({ kind: "rules", title: "Rules", files: rules });
  if (auto.length)
    groups.push({ kind: "auto", title: "Auto-memory", files: auto });
  return groups;
}

/* How a file is named in a list: a rule by its name under .claude/rules,
   an auto-memory file by its path, a root file by its path. */
export function memoryLabel(file: Pick<MemoryFile, "scope" | "path">) {
  if (file.scope === "workspace" && file.path.startsWith(".claude/rules/"))
    return file.path.slice(".claude/rules/".length);
  return file.path;
}

/* One line on what a file is for, for the list's secondary text. */
export function memoryDescription(file: Pick<MemoryFile, "scope" | "path">) {
  if (file.scope === "auto") return "kept by the agent's CLI between sessions";
  switch (file.path) {
    case "CLAUDE.md":
      return "project instructions for Claude";
    case "CLAUDE.local.md":
      return "local instructions, not committed";
    case "AGENTS.md":
      return "project instructions for Codex";
    case ".claude/CLAUDE.md":
      return "project instructions under .claude";
  }
  return "rule";
}

/* Which fixed files are not there yet, for the "create" affordances. */
export function missingWorkspaceFiles(files: MemoryFile[]) {
  const present = new Set(
    files.filter((f) => f.scope === "workspace").map((f) => f.path),
  );
  return WORKSPACE_FILES.filter((p) => !present.has(p));
}

const segment = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;

/* The runner's path rule: a fixed root file, a .md under .claude/rules at
   most three levels down, or (auto) a .md at most two levels down; no
   segment starts with a dot, so nothing hidden or ".." is reachable. Returns
   the reason a path is refused, or "" when it is fine. */
export function memoryPathError(scope: "workspace" | "auto", path: string) {
  if (
    !path ||
    path.length > 512 ||
    /[\0\n\r\\]/.test(path) ||
    path.startsWith("/")
  )
    return "Enter a relative path.";
  let segments = path.split("/");
  if (scope === "workspace") {
    if (WORKSPACE_FILES.includes(path)) return "";
    if (
      segments.length < 3 ||
      segments.length > 5 ||
      segments[0] !== ".claude" ||
      segments[1] !== "rules"
    )
      return "A workspace file is CLAUDE.md, CLAUDE.local.md, AGENTS.md, .claude/CLAUDE.md or a .md file under .claude/rules.";
    segments = segments.slice(2);
  } else if (segments.length > 3) {
    return "An auto-memory file is a .md file at most two levels under the memory directory.";
  }
  if (!segments.every((s) => segment.test(s)))
    return "Path segments are letters, digits, dots, dashes and underscores, and never start with a dot.";
  if (!segments[segments.length - 1].endsWith(".md"))
    return "Memory files are markdown (.md).";
  return "";
}

/* A rule's name as typed ("style" or "go/errors.md") to its path. */
export function rulePath(name: string) {
  const trimmed = name.trim().replace(/^\/+|\/+$/g, "");
  if (!trimmed) return "";
  return (
    ".claude/rules/" + (trimmed.endsWith(".md") ? trimmed : trimmed + ".md")
  );
}
