import { describe, expect, it } from "vitest";
import {
  environmentRows,
  kindLabel,
  memory,
  mergeReports,
  platform,
  sortReports,
  versionParts,
  type BugReport,
  type BugReportRow,
} from "./bugreports";

const row = (id: string, receivedAt: string): BugReportRow => ({
  id,
  receivedAt,
  kind: "error",
  component: "chat",
  version: "v0.1.0-alpha.12-210-g81d0bfc",
  os: "darwin",
  arch: "arm64",
  summary: "s",
});

describe("bug report lists", () => {
  it("sorts newest first, ties by id descending", () => {
    const sorted = sortReports([
      row("a", "2026-09-18T10:00:00Z"),
      row("c", "2026-09-18T11:00:00Z"),
      row("b", "2026-09-18T10:00:00Z"),
    ]);
    expect(sorted.map((r) => r.id)).toEqual(["c", "b", "a"]);
  });
  it("merges a further page without repeating an id", () => {
    const shown = [
      row("c", "2026-09-18T11:00:00Z"),
      row("b", "2026-09-18T10:30:00Z"),
    ];
    const page = [
      row("b", "2026-09-18T10:30:00Z"),
      row("a", "2026-09-18T10:00:00Z"),
    ];
    expect(mergeReports(shown, page).map((r) => r.id)).toEqual(["c", "b", "a"]);
  });
});

describe("bug report formatting", () => {
  it("labels kinds and platforms", () => {
    expect(kindLabel("error")).toBe("Error");
    expect(kindLabel("user")).toBe("User report");
    expect(kindLabel("other")).toBe("other");
    expect(platform("darwin", "arm64")).toBe("darwin/arm64");
    expect(platform("linux", undefined)).toBe("linux");
    expect(platform(undefined, undefined)).toBe("—");
  });
  it("splits a git-describe version into tag and sha", () => {
    expect(versionParts("v0.1.0-alpha.12-210-g81d0bfc")).toEqual({
      tag: "v0.1.0-alpha.12",
      sha: "81d0bfc",
    });
    expect(versionParts("v0.1.0-alpha.12-210-g81d0bfc-dirty")).toEqual({
      tag: "v0.1.0-alpha.12",
      sha: "81d0bfc-dirty",
    });
    expect(versionParts("v0.2.0")).toEqual({ tag: "v0.2.0", sha: "" });
    expect(versionParts(undefined)).toEqual({ tag: "—", sha: "" });
  });
  it("formats memory in GiB above a gibibyte", () => {
    expect(memory(32768)).toBe("32 GiB");
    expect(memory(1536)).toBe("1.5 GiB");
    expect(memory(512)).toBe("512 MiB");
    expect(memory(undefined)).toBe("");
  });
  it("lists only the environment a report carries, context ids last", () => {
    const report: BugReport = {
      schema: 1,
      id: "3f2a9c1e5b7d4a0c8e6f1b2d3c4a5e6f",
      kind: "error",
      component: "chat",
      trigger: "test",
      summary: "test exception",
      error: { message: "boom", operation: "bug-test" },
      warden: {
        version: "v0.1.0-alpha.12-210-g81d0bfc",
        protocol: 2,
        runtime: "sbx",
        installed: { codex: "0.154.0", claude: "2.1.272", guestArch: "arm64" },
      },
      system: {
        os: "darwin",
        arch: "arm64",
        osVersion: "24.6.0",
        cpus: 10,
        memoryMB: 32768,
      },
      context: {
        chatID: "8c1d2e3f",
        runID: "",
        provider: "claude",
        model: "claude-opus-5",
      },
      source: "0123456789abcdef0123456789abcdef",
    };
    const rows = environmentRows(report);
    expect(rows).toEqual([
      ["Warden", "v0.1.0-alpha.12-210-g81d0bfc"],
      ["Protocol", "2"],
      ["Runtime", "sbx"],
      ["Codex", "0.154.0"],
      ["Claude", "2.1.272"],
      ["Guest", "arm64"],
      ["System", "darwin/arm64 24.6.0"],
      ["CPUs", "10"],
      ["Memory", "32 GiB"],
      ["Trigger", "test"],
      ["Operation", "bug-test"],
      ["Source", "0123456789ab"],
      ["Chat", "8c1d2e3f"],
      ["Provider", "claude"],
      ["Model", "claude-opus-5"],
    ]);
    expect(
      environmentRows({
        schema: 1,
        id: "x",
        kind: "user",
        component: "cli",
        summary: "s",
      }),
    ).toEqual([]);
  });
});
