import { describe, it, expect } from "vitest";
import {
  COMMANDS,
  sideQuestion,
  agentCommandNamed,
  agentHint,
  attachArgs,
  attachCommand,
  attachInsert,
  attachQuery,
  bugReport,
  commandItems,
  exactCommand,
  isBugTest,
  isLocalPath,
  localMentions,
  mentionFor,
  mentionToken,
  parseThinking,
  resourceItems,
  resourceQuery,
  prefixed,
  quoteCommand,
  replaceTrigger,
  rewriteMentions,
  thinkingLabel,
  triggerAt,
  withoutCommand,
} from "./composer";
import { modelOptions } from "./models";

const models = [
  { value: "gpt-5.6-sol", label: "GPT-5.6 Sol", hint: "Default" },
  { value: "gpt-5.5", label: "GPT-5.5" },
  { value: "gpt-6-astra", label: "GPT-6 Astra" },
];

describe("permission mode command", () => {
  it("lists the modes after /mode and runs an exact one", () => {
    // "mode" is a prefix of "model" too; both commands are offered.
    expect(
      commandItems("mode", models).map((i) =>
        i.kind === "command" ? i.command.name : i.kind,
      ),
    ).toEqual(["model", "mode"]);
    expect(
      commandItems("mode ", models).map((i) =>
        i.kind === "mode" ? i.mode.value : i.kind,
      ),
    ).toEqual(["auto", "ask", "plan"]);
    expect(
      commandItems("mode p", models).map((i) =>
        i.kind === "mode" ? i.mode.value : i.kind,
      ),
    ).toEqual(["plan"]);
    const hit = exactCommand("/mode ask", models);
    expect(hit?.kind === "mode" && hit.mode.value).toBe("ask");
    expect(exactCommand("/mode bypass", models)).toBeUndefined();
    expect(exactCommand("/mode", models)).toEqual({
      kind: "command",
      command: COMMANDS.find((c) => c.name === "mode"),
    });
  });
});

describe("thinking and effort commands", () => {
  it("lists the presets after /thinking, takes any budget, and runs an exact one", () => {
    const values = (q: string) =>
      commandItems(q, models).map((i) =>
        i.kind === "thinking" ? i.thinking.value : i.kind,
      );
    expect(values("thinking ")).toEqual(["", "off", "4000", "16000", "32000"]);
    expect(values("thinking o")).toEqual(["", "off"]);
    expect(values("thinking 8k")).toEqual(["8000"]);
    expect(values("thinking 4k")).toEqual(["4000"]);
    expect(values("thinking lots")).toEqual([]);
    const hit = exactCommand("/thinking 8k", models);
    expect(hit?.kind === "thinking" && hit.thinking.value).toBe("8000");
    const off = exactCommand("/thinking off", models);
    expect(off?.kind === "thinking" && off.thinking.value).toBe("off");
    const on = exactCommand("/thinking on", models);
    expect(on?.kind === "thinking" && on.thinking.value).toBe("");
    expect(exactCommand("/thinking lots", models)).toBeUndefined();
    expect(parseThinking("16K")).toBe("16000");
    expect(parseThinking("0")).toBe("off");
    expect(parseThinking("-3")).toBeUndefined();
    expect(parseThinking("999999")).toBeUndefined();
    expect(thinkingLabel("8000")).toBe("8k");
    expect(thinkingLabel("1500")).toBe("1500");
    expect(thinkingLabel(undefined)).toBe("default");
  });
  it("lists the effort levels after /effort and runs an exact one", () => {
    const values = (q: string) =>
      commandItems(q, models).map((i) =>
        i.kind === "effort" ? i.effort.value : i.kind,
      );
    // No "default" row: a chat always runs a level (high until chosen).
    expect(values("effort ")).toEqual(["low", "medium", "high", "xhigh", "max"]);
    expect(values("effort m")).toEqual(["medium", "max"]);
    const hit = exactCommand("/effort xhigh", models);
    expect(hit?.kind === "effort" && hit.effort.value).toBe("xhigh");
    expect(exactCommand("/effort default", models)).toBeUndefined();
    expect(exactCommand("/effort ultra", models)).toBeUndefined();
  });
  it("lists the 1M-context models, disabled with a hint until the service allows them", () => {
    // The provider's default leads, marked; there is no "default" row.
    expect(modelOptions("claude").map((m) => m.value)).toEqual([
      "opus",
      "sonnet",
      "haiku",
      "sonnet[1m]",
      "opus[1m]",
    ]);
    expect(modelOptions("claude")[0].hint).toMatch(/^Default/);
    expect(
      modelOptions("claude")
        .filter((m) => m.disabled)
        .map((m) => m.value),
    ).toEqual(["sonnet[1m]", "opus[1m]"]);
    expect(
      modelOptions("claude", { fastMode: false, longContext: true }).some(
        (m) => m.disabled,
      ),
    ).toBe(false);
    expect(
      modelOptions("codex", { fastMode: true, longContext: true }),
    ).toHaveLength(5);
    // The /model rows carry the hint and the disabled state.
    const items = commandItems("model opus[", modelOptions("claude"));
    expect(items).toHaveLength(1);
    expect(items[0].kind === "model" && items[0].model.disabled).toBe(true);
  });
});

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
    ).toEqual(["gpt-5.6-sol", "gpt-5.5", "gpt-6-astra"]);
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
    // A label matches too ("sol" finds GPT-5.6 Sol); no default row exists.
    expect(
      commandItems("model sol", models).map((i) =>
        i.kind === "model" ? i.model.label : "",
      ),
    ).toEqual(["GPT-5.6 Sol"]);
    expect(commandItems("model prov", models)).toEqual([]);
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
      "mode",
      "thinking",
      "effort",
      "export",
      "rewind",
      "diff",
      "fork",
      "btw",
      "cost",
      "bug",
      "test",
      "style",
      "attach",
      "clear",
      "/compact",
      "/init",
      "/security-review",
      "/probe-cmd",
    ]);
    // The chat's "model" and "clear" shadow the agent's.
    expect(names("mo")).toEqual(["model", "mode"]);
    expect(names("c")).toEqual(["cost", "clear", "/compact"]);
    expect(names("Pro")).toEqual(["/probe-cmd"]);
    // With an argument there is nothing to pick: the text goes as it is.
    expect(names("compact focus on tests")).toEqual([]);
    expect(names("probe-cmd alpha")).toEqual([]);
    // Without agent commands the list is as before.
    expect(commandItems("c", models).map((i) => i.kind)).toEqual([
      "command",
      "command",
    ]);
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

describe("side questions and output styles", () => {
  it("reads the report after /bug and recognises /test bugreporting", () => {
    expect(bugReport("/bug the spinner never stops")).toBe(
      "the spinner never stops",
    );
    expect(bugReport("/BUG two\nlines")).toBe("two\nlines");
    expect(bugReport("/bug")).toBeUndefined();
    expect(bugReport("/bug   ")).toBeUndefined();
    expect(bugReport("/bugs no")).toBeUndefined();
    expect(bugReport("hello /bug inside")).toBeUndefined();
    expect(isBugTest("/test bugreporting")).toBe(true);
    expect(isBugTest("  /Test  BugReporting ")).toBe(true);
    expect(isBugTest("/test")).toBe(false);
    expect(isBugTest("/test bugreporting now")).toBe(false);
    expect(isBugTest("/testing bugreporting")).toBe(false);
  });
  it("reads the question after /btw", () => {
    expect(sideQuestion("/btw what did we decide?")).toBe(
      "what did we decide?",
    );
    expect(sideQuestion("/BTW  two\nlines ")).toBe("two\nlines");
    expect(sideQuestion("/btw")).toBeUndefined();
    expect(sideQuestion("/btw   ")).toBeUndefined();
    expect(sideQuestion("/btwx no")).toBeUndefined();
    expect(sideQuestion("btw no slash")).toBeUndefined();
    expect(sideQuestion("hello /btw inside")).toBeUndefined();
  });
  it("lists the styles once /style has its argument", () => {
    const kinds = (q: string) => commandItems(q, models).map((i) => i.kind);
    expect(kinds("style")).toEqual(["command"]);
    expect(
      commandItems("style ", models).map((i) =>
        i.kind === "style" ? i.style.label : i.kind,
      ),
    ).toEqual(["Default", "Explanatory", "Learning"]);
    expect(
      commandItems("style le", models).map((i) =>
        i.kind === "style" ? i.style.value : i.kind,
      ),
    ).toEqual(["Learning"]);
    const exact = exactCommand("/style explanatory", models);
    expect(exact?.kind === "style" && exact.style.value).toBe("Explanatory");
    const def = exactCommand("/style default", models);
    expect(def?.kind === "style" && def.style.value).toBe("");
    expect(exactCommand("/style nope", models)).toBeUndefined();
    // "/btw question" is never an exact local command: the question is asked.
    expect(exactCommand("/btw why?", models)).toBeUndefined();
    // Nor "/bug text" or "/test bugreporting": the report is drafted, the
    // exception raised.
    expect(exactCommand("/bug it broke", models)).toBeUndefined();
    expect(exactCommand("/test bugreporting", models)).toBeUndefined();
    expect(exactCommand("/bug", models)?.kind).toBe("command");
    expect(exactCommand("/test", models)?.kind).toBe("command");
    expect(exactCommand("/cost", models)?.kind).toBe("command");
  });
});

describe("resource mentions", () => {
  const resources = {
    documents: [
      { id: "1AbC", title: "Budget 2026", kind: "spreadsheet", access: "read" },
      { id: "2DeF", title: "", kind: "document", access: "" },
    ],
    repositories: [
      {
        name: "monaddle-too/warden",
        cloneURL: "https://github.com/monaddle-too/warden.git",
        access: ["contents"],
      },
    ],
    previews: [
      { id: "b1", title: "Dev server", port: 3000, url: "https://b1.example/" },
    ],
  };
  it("builds the token the service expands", () => {
    expect(mentionToken("doc", "Budget 2026")).toBe('@doc:"Budget 2026"');
    expect(mentionToken("repo", "monaddle-too/warden")).toBe(
      "@repo:monaddle-too/warden",
    );
    expect(mentionToken("preview", ' Say "hi" ')).toBe('@preview:"Say hi"');
    expect(mentionToken("preview", "")).toBe('@preview:""');
  });
  it("lists every resource on a bare @, one kind once typed", () => {
    expect(
      resourceItems(resources, "").map((r) => `${r.kind}:${r.name}|${r.insert}|${r.hint}`),
    ).toEqual([
      'doc:Budget 2026|@doc:"Budget 2026" |spreadsheet · read access',
      "doc:2DeF|@doc:2DeF |document",
      "repo:monaddle-too/warden|@repo:monaddle-too/warden |repository · contents",
      "preview:Dev server|@preview:\"Dev server\" |https://b1.example/",
    ]);
    expect(resourceItems(resources, "doc:bud").map((r) => r.name)).toEqual([
      "Budget 2026",
    ]);
    expect(resourceItems(resources, "REPO:").map((r) => r.name)).toEqual([
      "monaddle-too/warden",
    ]);
    expect(resourceItems(resources, "dev").map((r) => r.name)).toEqual([
      "Dev server",
    ]);
    expect(resourceItems(resources, "preview:zzz")).toEqual([]);
    expect(resourceItems(undefined, "")).toEqual([]);
  });
  it("knows a resource-only query", () => {
    expect(resourceQuery("doc:")).toBe(true);
    expect(resourceQuery("Repo:mon")).toBe(true);
    expect(resourceQuery("src/doc:x")).toBe(false);
    expect(resourceQuery("")).toBe(false);
  });
});

describe("files from this computer", () => {
  it("tells a local mention from a workspace one by its prefix", () => {
    expect(isLocalPath("./a")).toBe(true);
    expect(isLocalPath("../a")).toBe(true);
    expect(isLocalPath("~/a")).toBe(true);
    expect(isLocalPath("src/a")).toBe(false);
    expect(isLocalPath("~")).toBe(false);
    expect(isLocalPath(".hidden")).toBe(false);
  });

  it("lists the local mentions once, without trailing punctuation", () => {
    expect(
      localMentions(
        "read @~/notes.md, then @./a.txt and @~/notes.md; not me@./x nor @src/x.ts, @../up/y.go).",
      ),
    ).toEqual(["~/notes.md", "./a.txt", "../up/y.go"]);
    // A bare prefix names nothing.
    expect(localMentions("@~/ @./")).toEqual([]);
    expect(localMentions("nothing here")).toEqual([]);
  });

  it("rewrites attached mentions to their workspace paths", () => {
    expect(
      rewriteMentions("see @~/a.txt and @~/a.txt, plus @./*.log; @~/gone.txt", [
        { typed: "~/a.txt", path: ".warden/attachments/1.txt" },
        { typed: "./*.log", path: ".warden/attachments/2.log" },
        { typed: "./*.log", path: ".warden/attachments/3.log" },
      ]),
    ).toBe(
      "see @.warden/attachments/1.txt and @.warden/attachments/1.txt, plus @.warden/attachments/2.log @.warden/attachments/3.log; @~/gone.txt",
    );
  });

  it("splits an /attach line into paths, quoted for spaces", () => {
    expect(attachArgs(` ~/a.txt "My Docs/b c.pdf" './x y' *.log`)).toEqual([
      "~/a.txt",
      "My Docs/b c.pdf",
      "./x y",
      "*.log",
    ]);
    expect(attachArgs('"unterminated a')).toEqual(["unterminated a"]);
    expect(attachArgs("")).toEqual([]);
    expect(attachCommand("/attach ~/a.txt b")).toEqual(["~/a.txt", "b"]);
    expect(attachCommand("/ATTACH  x ")).toEqual(["x"]);
    expect(attachCommand("/attach")).toBeUndefined();
    expect(attachCommand("/attach   ")).toBeUndefined();
    expect(attachCommand("/attach a\nmore")).toBeUndefined();
    expect(attachCommand("/attachments")).toBeUndefined();
    expect(attachCommand("attach a")).toBeUndefined();
  });

  it("finds the path being typed on an /attach line", () => {
    expect(attachQuery("/attach", 7)).toBeUndefined();
    expect(attachQuery("/atta", 5)).toBeUndefined();
    expect(attachQuery("/attach ", 8)).toEqual({ query: "", start: 8, end: 8 });
    expect(attachQuery("/attach ~/Doc", 13)).toEqual({
      query: "~/Doc",
      start: 8,
      end: 13,
    });
    // The caret in the middle of a word: the query is the part before it,
    // the whole word is replaced.
    expect(attachQuery("/attach ~/Docs/x.txt b", 11)).toEqual({
      query: "~/D",
      start: 8,
      end: 20,
    });
    expect(attachQuery("/attach a b", 11)).toEqual({ query: "b", start: 10, end: 11 });
    // A quoted word: the query is what is inside it.
    expect(attachQuery(`/attach "~/My Docs/re`, 21)).toEqual({
      query: "~/My Docs/re",
      start: 8,
      end: 21,
    });
    expect(attachQuery(`/attach "~/My Docs/a.txt" `, 26)).toEqual({
      query: "",
      start: 26,
      end: 26,
    });
    // Off the first line, nothing.
    expect(attachQuery("/attach a\nb", 11)).toBeUndefined();
  });

  it("inserts a picked path quoted when it needs it", () => {
    expect(attachInsert("~/a.txt")).toBe("~/a.txt ");
    expect(attachInsert("~/Docs/")).toBe("~/Docs/");
    expect(attachInsert("~/My Docs/a.txt")).toBe('"~/My Docs/a.txt" ');
    expect(attachInsert("~/My Docs/")).toBe('"~/My Docs/');
  });

  it("offers /attach in the command list", () => {
    expect(
      commandItems("att", models).map((i) => i.kind === "command" && i.command.name),
    ).toEqual(["attach"]);
    expect(exactCommand("/attach", models)).toMatchObject({
      kind: "command",
      command: { name: "attach" },
    });
    expect(exactCommand("/attach ~/a", models)).toBeUndefined();
  });
});
