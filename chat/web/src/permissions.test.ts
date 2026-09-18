import { describe, it, expect } from "vitest";
import {
  PERMISSION_METHOD,
  alwaysHint,
  alwaysLabel,
  askedCommand,
  isPlan,
  permissionParams,
  permissionTitle,
} from "./permissions";
import type { Approval, Entry } from "./types";

const entry = (over: Partial<Entry>): Entry => ({
  id: "toolu_1",
  role: "activity",
  text: "",
  detail: "",
  createdAt: 0,
  isStreaming: false,
  delivery: "",
  ...over,
});
const approval = (
  params: Record<string, unknown>,
  method = PERMISSION_METHOD,
): Approval => ({
  id: "a",
  method,
  state: "pending",
  params,
});

describe("permission asks", () => {
  it("recognises the ask by its method and tool", () => {
    expect(permissionParams(approval({ tool: "Bash" }))?.tool).toBe("Bash");
    expect(
      permissionParams(approval({ port: 3000 }, "warden/ports/bind")),
    ).toBeUndefined();
    expect(permissionParams(approval({}))).toBeUndefined();
  });
  it("titles a command, an edit and a plan in the person's terms", () => {
    const command = {
      tool: "Bash",
      input: { command: "touch x" },
      always: "`touch` commands",
      entry: entry({
        text: "touch x",
        tool: { kind: "command", name: "Bash", status: "running" },
      }),
    };
    expect(permissionTitle(command)).toBe("Run a command");
    expect(askedCommand(command)).toBe("touch x");
    expect(alwaysLabel(command)).toBe(
      "Always allow `touch` commands in this chat",
    );
    expect(alwaysLabel(command, "workspace")).toBe(
      "Always allow `touch` commands in this workspace",
    );
    expect(alwaysHint({ ...command, rule: "Bash(touch *)" }, "workspace")).toBe(
      "Allow `touch` commands without asking in every chat of this workspace, present and future — the rule Bash(touch *)",
    );
    const edit = {
      tool: "Write",
      entry: entry({
        text: "Write notes.md",
        tool: {
          kind: "edit",
          name: "Write",
          status: "running",
          paths: ["notes.md"],
        },
      }),
    };
    expect(permissionTitle(edit)).toBe("Write notes.md");
    expect(askedCommand(edit)).toBe("");
    expect(alwaysLabel(edit)).toBe("Allow always in this chat");
    expect(alwaysHint(edit)).toBe(
      "Allow this without asking in the rest of this chat",
    );
    const plan = { tool: "ExitPlanMode", plan: "# Plan" };
    expect(isPlan(plan)).toBe(true);
    expect(permissionTitle(plan)).toBe("Claude has a plan");
    expect(permissionTitle({ tool: "Mystery" })).toBe("Use Mystery");
  });
});
