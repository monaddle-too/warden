import { describe, it, expect } from "vitest";
import {
  authorLabel,
  exportEntries,
  exportJSON,
  exportMarkdown,
  exportName,
  fenceFor,
  senderLabel,
} from "./export";
import type { Chat, Entry } from "./types";

const entry = (over: Partial<Entry>): Entry => ({
  id: "e",
  role: "assistant",
  text: "",
  detail: "",
  createdAt: 1_789_000_000,
  isStreaming: false,
  delivery: "",
  ...over,
});
const chat: Chat = {
  id: "c1",
  title: "Fix the build: part 2",
  provider: "claude",
  model: "opus",
  sandboxID: "s",
  repository: "github://owner/repo",
  status: "idle",
  archived: false,
  approvals: [],
  conversation: {
    threadID: "t",
    entries: [
      entry({
        id: "u",
        role: "user",
        text: "Please fix it",
        attachments: [
          {
            id: "a",
            name: "shot.png",
            path: ".warden/attachments/a.png",
            kind: "image",
            size: 2048,
          },
        ],
      }),
      entry({ id: "s", role: "activity", text: "Ran tests", detail: "ok\n" }),
      entry({ id: "r", role: "assistant", text: "# Done\n\nFixed." }),
      entry({ id: "m", role: "system", text: "Turn ended\nearly" }),
      entry({ id: "i", role: "image", text: "The result", detail: "img" }),
      entry({
        id: "f",
        role: "user",
        text: "Again",
        delivery: "failed",
        detail: "Service down",
      }),
    ],
  },
};
const at = new Date(Date.UTC(2026, 8, 17, 13, 5));
const time = (s: number) => `T${s}`;

describe("chat export", () => {
  it("names the author of an entry", () => {
    expect(senderLabel()).toBe("You");
    expect(senderLabel({ principalID: "owner" })).toBe("You");
    expect(senderLabel({ principalID: "x" })).toBe("Collaborator");
    expect(senderLabel({ principalID: "x", email: "a@b" })).toBe("a@b");
    expect(senderLabel({ principalID: "x", email: "a@b", name: "Ann" })).toBe(
      "Ann",
    );
    expect(authorLabel(entry({ role: "assistant" }), "claude")).toBe("Claude");
    expect(authorLabel(entry({ role: "assistant" }))).toBe("Codex");
  });
  it("picks a fence the text cannot close", () => {
    expect(fenceFor("plain")).toBe("```");
    expect(fenceFor("a\n```\nb")).toBe("````");
    expect(fenceFor("`````x")).toBe("``````");
  });
  it("leaves tool steps out unless asked", () => {
    const roles = (activity: boolean) =>
      exportEntries(chat.conversation.entries, activity).map((e) => e.role);
    expect(roles(false)).toEqual([
      "user",
      "assistant",
      "system",
      "image",
      "user",
    ]);
    expect(roles(true)).toContain("activity");
  });
  it("writes messages verbatim as markdown and describes the rest", () => {
    const md = exportMarkdown(
      chat,
      { format: "markdown", activity: true },
      at,
      time,
    );
    expect(md).toBe(
      [
        "# Fix the build: part 2",
        "",
        "- Agent: Claude (opus)",
        "- Repository: github://owner/repo",
        `- Exported: T${at.getTime() / 1000}`,
        "",
        "---",
        "",
        "## You — T1789000000",
        "",
        "Please fix it",
        "",
        "Attachments: `shot.png` (2 KB, `.warden/attachments/a.png`)",
        "",
        "### Activity — Ran tests",
        "",
        "```",
        "ok",
        "```",
        "",
        "## Claude — T1789000000",
        "",
        "# Done",
        "",
        "Fixed.",
        "",
        "> Turn ended",
        "> early",
        "",
        "_Image: The result_",
        "",
        "## You — T1789000000",
        "",
        "_Not delivered: Service down_",
        "",
        "Again",
        "",
      ].join("\n"),
    );
    const plain = exportMarkdown(
      chat,
      { format: "markdown", activity: false },
      at,
      time,
    );
    expect(plain).not.toContain("### Activity");
    expect(plain.endsWith("Again\n")).toBe(true);
  });
  it("fences activity output with a longer fence than it contains", () => {
    const md = exportMarkdown(
      {
        ...chat,
        conversation: {
          entries: [
            entry({ role: "activity", text: "cat", detail: "```js\nx\n```" }),
          ],
        },
      },
      { format: "markdown", activity: true },
      at,
      time,
    );
    expect(md).toContain("````\n```js\nx\n```\n````\n");
  });
  it("exports JSON with the chat's records under a named format", () => {
    const parsed = JSON.parse(
      exportJSON(chat, { format: "json", activity: false }, at),
    );
    expect(parsed.format).toBe("warden-chat");
    expect(parsed.exportedAt).toBe("2026-09-17T13:05:00.000Z");
    expect(parsed.chat).toEqual({
      id: "c1",
      title: "Fix the build: part 2",
      provider: "claude",
      model: "opus",
      repository: "github://owner/repo",
      status: "idle",
      archived: false,
      threadID: "t",
    });
    expect(parsed.entries.map((e: Entry) => e.id)).toEqual([
      "u",
      "r",
      "m",
      "i",
      "f",
    ]);
    expect(parsed.entries[0].attachments[0].name).toBe("shot.png");
  });
  it("names the file after the title, the time and the format", () => {
    const local = new Date(2026, 8, 17, 9, 7);
    expect(exportName("Fix the build: part 2", "markdown", local)).toBe(
      "fix-the-build-part-2-20260917-0907.md",
    );
    expect(exportName("  ../../étoile  ", "json", local)).toBe(
      "toile-20260917-0907.json",
    );
    expect(exportName("", "markdown", local)).toBe("chat-20260917-0907.md");
    expect(exportName("x".repeat(80), "markdown", local)).toBe(
      "x".repeat(60) + "-20260917-0907.md",
    );
  });
});
