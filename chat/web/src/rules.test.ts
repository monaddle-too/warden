import { describe, it, expect } from "vitest";
import {
  actorLabel,
  decidedBy,
  historySummary,
  kindHint,
  newestFirst,
  ruleError,
  ruleOrigin,
} from "./rules";
import type { PermissionEvent, Rule } from "./types";

describe("permission rules", () => {
  it("accepts Claude's rule syntax and words what is wrong", () => {
    for (const ok of [
      "Bash(git *)",
      "Bash(npm run test:*)",
      "Edit(src/**)",
      "Read",
      "WebFetch(domain:example.com)",
      "mcp__warden__*",
      " Write(/etc/**) ",
    ])
      expect(ruleError(ok), ok).toBe("");
    expect(ruleError("")).toMatch(/Write a pattern/);
    expect(ruleError("Bash(")).toMatch(/closing parenthesis/);
    expect(ruleError("Bash()")).toMatch(/Empty parentheses/);
    expect(ruleError("(x)")).toMatch(/tool's name/);
    expect(ruleError("Bash x")).toMatch(/not a tool name/);
    expect(ruleError("TodoWrite(x)")).toMatch(/take no argument/);
    expect(ruleError("WebFetch(example.com)")).toMatch(/domain:HOST/);
    expect(ruleError("Bash(a\nb)")).toMatch(/One line/);
  });
  it("says where a rule came from and by whom", () => {
    const chats = [{ id: "c1", title: "Fix the build" }];
    expect(
      ruleOrigin(
        {
          id: "r",
          kind: "allow",
          pattern: "Bash(git *)",
          origin: "always",
          chatID: "c1",
          by: { principalID: "p", name: "Dan" },
        },
        chats,
      ),
    ).toBe("Allow always in “Fix the build” by Dan");
    expect(
      ruleOrigin({
        id: "r",
        kind: "deny",
        pattern: "Bash(rm *)",
        origin: "editor",
        by: { principalID: "owner" },
      }),
    ).toBe("from the editor by the owner");
    expect(actorLabel({ principalID: "p", email: "a@b.c" })).toBe("a@b.c");
    expect(actorLabel(undefined)).toBe("");
    expect(kindHint("deny")).toMatch(/every mode/);
    expect(kindHint("nope")).toBe("");
  });
  it("words each decision and orders the history", () => {
    const rule: Rule = { id: "r", kind: "deny", pattern: "Bash(rm *)" };
    const events: PermissionEvent[] = [
      { id: "1", at: 10, tool: "Bash", summary: "touch x", decision: "allow", how: "auto" },
      { id: "2", at: 30, tool: "Bash", summary: "rm x", decision: "deny", how: "rule", rule, scope: "workspace" },
      { id: "3", at: 20, tool: "Write", summary: "a.md", decision: "allow", how: "card", by: { principalID: "p", name: "Dan" }, rule: { id: "q", kind: "allow", pattern: "Edit" }, scope: "chat" },
      { id: "4", at: 40, tool: "Bash", summary: "curl x", decision: "deny", how: "card", by: { principalID: "owner" }, message: "use the proxy" },
    ];
    expect(decidedBy(events[0])).toBe("auto mode");
    expect(decidedBy(events[1])).toBe("workspace rule deny Bash(rm *)");
    expect(decidedBy(events[2])).toBe("Dan, always for the chat (Edit)");
    expect(decidedBy(events[3])).toBe("the owner: use the proxy");
    expect(decidedBy({ ...events[1], rule: undefined })).toBe("a rule");
    expect(decidedBy({ ...events[3], by: undefined, message: "" })).toBe("a card");
    expect(newestFirst(events).map((e) => e.id)).toEqual(["4", "2", "3", "1"]);
    expect(historySummary(events)).toBe("2 allowed · 2 denied · 1 by rules");
    expect(historySummary([])).toMatch(/No tool asks/);
  });
});
