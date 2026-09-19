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
    turns: [
      {
        id: "turn",
        startedAt: 1_789_000_001,
        endedAt: 1_789_000_065,
        usage: {
          input: 1200,
          cached: 800,
          output: 300,
          total: 1500,
          costUSD: 0.02,
        },
      },
    ],
    entries: [
      entry({
        id: "u",
        role: "user",
        text: "Please fix it",
        turnID: "turn",
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
      entry({
        id: "r",
        role: "assistant",
        text: "# Done\n\nFixed.",
        turnID: "turn",
      }),
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
        "_Turn: 1m 05s · 1.5k tokens (1.2k in, 300 out) · $0.02_",
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
  it("quotes the model's thinking as a step, left out with the steps", () => {
    const thought = {
      ...chat,
      conversation: {
        entries: [
          entry({ role: "thinking", text: "**Plan**\n\nRead, then test." }),
          entry({ id: "m", text: "Done." }),
        ],
      },
    };
    const md = exportMarkdown(
      thought,
      { format: "markdown", activity: true },
      at,
      time,
    );
    expect(md).toContain(
      "### Thinking\n\n> **Plan**\n> \n> Read, then test.\n",
    );
    expect(
      exportMarkdown(
        thought,
        { format: "markdown", activity: false },
        at,
        time,
      ),
    ).not.toContain("Thinking");
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
    expect(parsed.turns).toEqual(chat.conversation.turns);
  });
  it("notes what a turn took under its last message", () => {
    const md = exportMarkdown(
      chat,
      { format: "markdown", activity: false },
      at,
      time,
    );
    expect(md).toContain(
      "Fixed.\n\n_Turn: 1m 05s · 1.5k tokens (1.2k in, 300 out) · $0.02_\n",
    );
    const bare = {
      ...chat,
      conversation: { entries: chat.conversation.entries },
    };
    expect(
      exportMarkdown(bare, { format: "markdown", activity: false }, at, time),
    ).not.toContain("_Turn:");
  });
  it("nests a subagent's work under its card and keeps the person's commands", () => {
    const nested = {
      ...chat,
      conversation: {
        entries: [
          entry({ id: "u", role: "user", text: "Look" }),
          entry({
            id: "agent",
            role: "activity",
            text: "Agent: look around (Explore)",
            detail: "It is in lex.go.",
            tool: { kind: "task", name: "Agent", status: "completed" },
          }),
          entry({
            id: "grep",
            role: "activity",
            text: 'Grep "tokenizer" in .',
            detail: "lex.go:12\n",
            parentID: "agent",
            tool: { kind: "search", name: "Grep", status: "completed" },
          }),
          entry({
            id: "inner",
            role: "activity",
            text: "Agent: dig (Plan)",
            parentID: "agent",
            tool: { kind: "task", name: "Agent", status: "completed" },
          }),
          entry({
            id: "deep",
            role: "activity",
            text: "Read lex.go",
            detail: "```\nx\n```",
            parentID: "inner",
            tool: { kind: "read", name: "Read", status: "completed" },
          }),
          entry({
            id: "said",
            role: "assistant",
            text: "Found it.",
            parentID: "agent",
          }),
          entry({ id: "r", role: "assistant", text: "It is in lex.go." }),
          entry({
            id: "mine",
            role: "activity",
            text: "git status",
            detail: "clean\n",
            sender: { principalID: "owner" },
            tool: { kind: "command", name: "Bash", status: "completed" },
          }),
        ],
      },
    };
    expect(
      exportEntries(nested.conversation.entries, true).map((e) => e.id),
    ).toEqual(["u", "agent", "grep", "inner", "deep", "said", "r", "mine"]);
    // The card left out takes its subagent's work with it; the person's
    // command stays.
    expect(
      exportEntries(nested.conversation.entries, false).map((e) => e.id),
    ).toEqual(["u", "r", "mine"]);
    const md = exportMarkdown(
      nested,
      { format: "markdown", activity: true },
      at,
      time,
    );
    expect(md).toContain(
      [
        "### Activity — Agent: look around (Explore)",
        "",
        '> ### Activity — Grep "tokenizer" in .',
        ">",
        "> ```",
        "> lex.go:12",
        "> ```",
        ">",
        "> ### Activity — Agent: dig (Plan)",
        ">",
        "> > ### Activity — Read lex.go",
        "> >",
        "> > ````",
        "> > ```",
        "> > x",
        "> > ```",
        "> > ````",
        "> >",
        ">",
        "> ## Claude — T1789000000",
        ">",
        "> Found it.",
        ">",
        "",
        "```",
        "It is in lex.go.",
        "```",
        "",
        "## Claude — T1789000000",
      ].join("\n"),
    );
    expect(md).toContain(
      "### Command by You — git status\n\n```\nclean\n```\n",
    );
    expect(
      exportMarkdown(nested, { format: "markdown", activity: false }, at, time),
    ).toContain("### Command by You — git status");
    const parsed = JSON.parse(
      exportJSON(nested, { format: "json", activity: true }, at),
    );
    expect(
      parsed.entries.map((e: Entry & { children?: Entry[] }) => [
        e.id,
        e.children?.map((c) => c.id),
      ]),
    ).toEqual([
      ["u", undefined],
      ["agent", ["grep", "inner", "said"]],
      ["r", undefined],
      ["mine", undefined],
    ]);
    expect(
      parsed.entries[1].children[1].children.map((e: Entry) => e.id),
    ).toEqual(["deep"]);
    expect(parsed.entries[1].children[0].parentID).toBe("agent");
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
