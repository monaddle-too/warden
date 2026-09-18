import { describe, expect, it } from "vitest";
import {
  groupMemory,
  memoryDescription,
  memoryLabel,
  memoryPathError,
  missingWorkspaceFiles,
  rulePath,
} from "./memory";
import type { MemoryFile } from "./types";

const file = (scope: MemoryFile["scope"], path: string): MemoryFile => ({
  scope,
  path,
  size: 1,
  text: "x",
});

describe("memory files", () => {
  it("groups root files, rules and auto-memory, empty groups left out", () => {
    const groups = groupMemory([
      file("workspace", ".claude/rules/style.md"),
      file("auto", "MEMORY.md"),
      file("workspace", "AGENTS.md"),
      file("workspace", "CLAUDE.md"),
      file("workspace", ".claude/rules/go/errors.md"),
      file("auto", "topics/go.md"),
    ]);
    expect(groups.map((g) => g.kind)).toEqual([
      "instructions",
      "rules",
      "auto",
    ]);
    expect(groups[0].files.map((f) => f.path)).toEqual([
      "CLAUDE.md",
      "AGENTS.md",
    ]);
    expect(groups[1].files.map((f) => f.path)).toEqual([
      ".claude/rules/style.md",
      ".claude/rules/go/errors.md",
    ]);
    expect(groups[2].files.map((f) => f.path)).toEqual([
      "MEMORY.md",
      "topics/go.md",
    ]);
    expect(groupMemory([file("auto", "MEMORY.md")]).map((g) => g.kind)).toEqual(
      ["auto"],
    );
    expect(groupMemory([])).toEqual([]);
  });

  it("labels and describes files", () => {
    expect(memoryLabel(file("workspace", ".claude/rules/go/errors.md"))).toBe(
      "go/errors.md",
    );
    expect(memoryLabel(file("workspace", "CLAUDE.md"))).toBe("CLAUDE.md");
    expect(memoryLabel(file("auto", "topics/go.md"))).toBe("topics/go.md");
    expect(memoryDescription(file("workspace", "CLAUDE.md"))).toContain(
      "Claude",
    );
    expect(memoryDescription(file("workspace", "AGENTS.md"))).toContain(
      "Codex",
    );
    expect(memoryDescription(file("workspace", ".claude/rules/x.md"))).toBe(
      "rule",
    );
    expect(memoryDescription(file("auto", "MEMORY.md"))).toContain("CLI");
  });

  it("says which root files are missing", () => {
    expect(
      missingWorkspaceFiles([
        file("workspace", "CLAUDE.md"),
        file("auto", "AGENTS.md"),
      ]),
    ).toEqual(["CLAUDE.local.md", "AGENTS.md", ".claude/CLAUDE.md"]);
  });

  it("mirrors the runner's path rule", () => {
    for (const ok of [
      "CLAUDE.md",
      "AGENTS.md",
      ".claude/CLAUDE.md",
      ".claude/rules/style.md",
      ".claude/rules/go/errors.md",
      ".claude/rules/a/b/c.md",
    ])
      expect(memoryPathError("workspace", ok), ok).toBe("");
    for (const bad of [
      "",
      "README.md",
      "../CLAUDE.md",
      "/CLAUDE.md",
      ".claude/rules/.hidden.md",
      ".claude/rules/x.txt",
      ".claude/rules/a/b/c/d.md",
      ".claude/settings.json",
      "src/CLAUDE.md",
    ])
      expect(memoryPathError("workspace", bad), bad).not.toBe("");
    for (const ok of ["MEMORY.md", "topics/go.md"])
      expect(memoryPathError("auto", ok), ok).toBe("");
    for (const bad of [
      "../MEMORY.md",
      ".hidden.md",
      "a/b/c/d.md",
      "MEMORY.txt",
    ])
      expect(memoryPathError("auto", bad), bad).not.toBe("");
  });

  it("turns a rule name into its path", () => {
    expect(rulePath("style")).toBe(".claude/rules/style.md");
    expect(rulePath(" go/errors.md ")).toBe(".claude/rules/go/errors.md");
    expect(rulePath("/x/")).toBe(".claude/rules/x.md");
    expect(rulePath("  ")).toBe("");
  });
});
