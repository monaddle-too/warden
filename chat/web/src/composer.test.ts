import { describe, it, expect } from "vitest";
import {
  COMMANDS,
  agentCommandNamed,
  agentHint,
  commandItems,
  exactCommand,
  mentionFor,
  prefixed,
  quoteCommand,
  replaceTrigger,
  triggerAt,
  withoutCommand,
} from "./composer";

const models = [
  { value: "", label: "Provider default" },
  { value: "gpt-5.5", label: "GPT-5.5" },
  { value: "gpt-6-astra", label: "GPT-6 Astra" },
];

describe("composer triggers", () => {
  it("reads a leading slash as a command up to the caret on the first line", () => {
    expect(triggerAt("/", 1)).toEqual({
      kind: "command",
      start: 0,
      end: 1,
      query: "",
    });
    expect(triggerAt("/model gpt", 10)).toEqual({
      kind: "command",
      start: 0,
      end: 10,
      query: "model gpt",
    });
    expect(triggerAt("/model gpt", 3)?.query).toBe("mo");
    // Below the first line, or with the caret before the slash, it is text.
    expect(triggerAt("/stop\nand more", 8)).toBeUndefined();
    expect(triggerAt("/stop", 0)).toBeUndefined();
    expect(triggerAt("not /stop", 9)).toBeUndefined();
  });
  it("reads an @ at the start of a word as a path prefix", () => {
    expect(triggerAt("see @src/co", 11)).toEqual({
      kind: "path",
      start: 4,
      end: 11,
      query: "src/co",
    });
    expect(triggerAt("@", 1)).toEqual({
      kind: "path",
      start: 0,
      end: 1,
      query: "",
    });
    // The caret inside the word completes what is before it and the pick
    // replaces the whole word.
    expect(triggerAt("@src/components/x rest", 5)).toEqual({
      kind: "path",
      start: 0,
      end: 17,
      query: "src/",
    });
    // An address is not a mention, nor is the caret before the @.
    expect(triggerAt("mail me@example.com", 19)).toBeUndefined();
    expect(triggerAt("x @src", 2)).toBeUndefined();
    expect(triggerAt("x @src y", 8)).toBeUndefined();
  });
  it("lists commands by prefix and models by substring once model has an argument", () => {
    expect(commandItems("", models).map((i) => i.kind)).toEqual(
      COMMANDS.map(() => "command"),
    );
    expect(
      commandItems("Ex", models).map((i) =>
        i.kind === "command" ? i.command.name : "",
      ),
    ).toEqual(["export"]);
    expect(commandItems("zzz", models)).toEqual([]);
    expect(commandItems("model", models)).toHaveLength(1);
    expect(
      commandItems("model ", models).map((i) =>
        i.kind === "model" ? i.model.value : "",
      ),
    ).toEqual(["", "gpt-5.5", "gpt-6-astra"]);
    expect(
      commandItems("model GPT-6", models).map((i) =>
        i.kind === "model" ? i.model.value : "",
      ),
    ).toEqual(["gpt-6-astra"]);
    expect(
      commandItems("model 5.5", models).map((i) =>
        i.kind === "model" ? i.model.value : "",
      ),
    ).toEqual(["gpt-5.5"]);
    expect(
      commandItems("model prov", models).map((i) =>
        i.kind === "model" ? i.model.label : "",
      ),
    ).toEqual(["Provider default"]);
    expect(commandItems("stop now", models)).toEqual([]);
  });
  it("recognises a message that is exactly a command", () => {
    expect(exactCommand("/stop", models)).toEqual({
      kind: "command",
      command: COMMANDS[0],
    });
    expect(exactCommand("  /Export  ", models)?.kind).toBe("command");
    expect(exactCommand("/model gpt-5.5", models)).toEqual({
      kind: "model",
      model: models[1],
    });
    expect(exactCommand("/model gpt-6 astra", models)).toEqual({
      kind: "model",
      model: models[2],
    });
    expect(exactCommand("/model gpt", models)).toBeUndefined();
    expect(exactCommand("/stop the agent", models)).toBeUndefined();
    expect(exactCommand("/stop\nplease", models)).toBeUndefined();
    expect(exactCommand("stop", models)).toBeUndefined();
  });
  it("replaces the trigger with the pick and places the caret", () => {
    const trigger = triggerAt("open @src/co please", 12)!;
    expect(
      replaceTrigger(
        "open @src/co please",
        trigger,
        mentionFor("src/components/"),
      ),
    ).toEqual({
      text: "open @src/components/ please",
      caret: 21,
    });
    expect(mentionFor("src/conv.ts")).toBe("@src/conv.ts ");
    expect(
      withoutCommand("/stop\nkeep this", triggerAt("/stop\nkeep this", 5)!),
    ).toBe("keep this");
    expect(withoutCommand("/stop", triggerAt("/stop", 5)!)).toBe("");
  });
});

/* The agent's own commands, as Claude Code's session lists them (the
   built-ins and a workspace command). */
const agent = [
  { name: "compact" },
  { name: "init" },
  { name: "model" },
  { name: "clear" },
  { name: "security-review" },
  { name: "probe-cmd", description: "Probe command from the workspace" },
];

describe("agent commands in the composer", () => {
  it("lists the agent's commands after the chat's, by prefix, local names first", () => {
    const names = (query: string) =>
      commandItems(query, models, agent).map((i) =>
        i.kind === "command"
          ? i.command.name
          : i.kind === "agent"
            ? "/" + i.command.name
            : "",
      );
    expect(names("")).toEqual([
      "stop",
      "model",
      "export",
      "clear",
      "/compact",
      "/init",
      "/security-review",
      "/probe-cmd",
    ]);
    // The chat's "model" and "clear" shadow the agent's.
    expect(names("mo")).toEqual(["model"]);
    expect(names("c")).toEqual(["clear", "/compact"]);
    expect(names("Pro")).toEqual(["/probe-cmd"]);
    // With an argument there is nothing to pick: the text goes as it is.
    expect(names("compact focus on tests")).toEqual([]);
    expect(names("probe-cmd alpha")).toEqual([]);
    // Without agent commands the list is as before.
    expect(commandItems("c", models).map((i) => i.kind)).toEqual(["command"]);
  });
  it("names the agent command a query is for, argument or not", () => {
    expect(agentCommandNamed("compact", agent)?.name).toBe("compact");
    expect(agentCommandNamed("compact focus on tests", agent)?.name).toBe(
      "compact",
    );
    expect(agentCommandNamed("probe-cmd alpha beta", agent)?.description).toBe(
      "Probe command from the workspace",
    );
    expect(agentCommandNamed("comp", agent)).toBeUndefined();
    expect(agentCommandNamed("model opus", agent)).toBeUndefined();
    expect(agentCommandNamed("", agent)).toBeUndefined();
    expect(agentCommandNamed("no-such", agent)).toBeUndefined();
  });
  it("never runs an agent command locally: exactly /compact is sent as text", () => {
    expect(exactCommand("/compact", models)).toBeUndefined();
    expect(exactCommand("/init", models)).toBeUndefined();
    expect(exactCommand("/probe-cmd alpha", models)).toBeUndefined();
    expect(exactCommand("/stop", models)?.kind).toBe("command");
  });
  it("hints known built-ins and prefers the agent's description", () => {
    expect(agentHint({ name: "compact" })).toMatch(/context/);
    expect(agentHint({ name: "init" })).toMatch(/CLAUDE\.md/);
    expect(agentHint({ name: "init", description: "Own words" })).toBe(
      "Own words",
    );
    expect(agentHint({ name: "goal" })).toBe("");
  });
});

describe("! and # prefixes", () => {
  it("reads a leading ! as a shell command and # as a memory note", () => {
    expect(prefixed("!ls -la")).toEqual({ kind: "shell", command: "ls -la" });
    expect(prefixed("!  git status\n")).toEqual({
      kind: "shell",
      command: "git status",
    });
    expect(prefixed("#use tabs")).toEqual({ kind: "memory", note: "use tabs" });
    expect(prefixed("# use tabs\nnot spaces")).toEqual({
      kind: "memory",
      note: "use tabs\nnot spaces",
    });
    for (const text of [
      "!",
      "#",
      "! ",
      "hello !world",
      " #x",
      "a\n!b",
      "/stop",
      "",
    ])
      expect(prefixed(text), text).toBeUndefined();
  });

  it("quotes a command card into a message for the agent", () => {
    expect(
      quoteCommand({
        text: "git status",
        detail: "On branch main\nnothing to commit\n",
        tool: { status: "completed" },
      }),
    ).toBe(
      "I ran `git status` in the workspace:\n```\nOn branch main\nnothing to commit\n```\n",
    );
    expect(
      quoteCommand({ text: "false", detail: "", tool: { status: "exit 1" } }),
    ).toBe("I ran `false` in the workspace (exit 1); it printed nothing.\n");
    expect(quoteCommand({ text: "true", detail: "" })).toBe(
      "I ran `true` in the workspace; it printed nothing.\n",
    );
  });
});
