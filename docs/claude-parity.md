# Claude parity: what the chat and the TUI should support

Branch `feat/claude-parity`, worktree `.local/warden-claude-parity`, from
main 21615a0 (2026-09-17).

## Objective

Make Warden's own two surfaces — the web chat and `warden chat`'s terminal
client (`chat/internal/tui`) — good enough that a person who would reach for
the Claude Code TUI does not need to. Both surfaces render the same
conversation model (`chat/internal/conversation`), fed by the Claude adapter
(`chat/internal/agent/claude.go`, the SDK stream-json control protocol) and
the Codex adapter (`agent/rpc.go`, app-server), so almost every item below
is one adapter or engine change plus a rendering on each surface. Whatever
is built serves both providers.

## Why parity, not a pass-through

Considered and set aside (2026-09-17):

- **A pty into the sandbox running the Claude TUI** (`warden remote chat
  <chat> --shell claude`), with Warden mirroring the session back into the
  chat by tailing `~/.claude/projects/*.jsonl`, hooks and `/proc`. Rejected
  for now: the mirror consumes unversioned CLI internals that change with
  every pinned release; the web transcript becomes a delayed replay (no
  deltas, unclear thinking/usage); Claude's permission prompts leave the
  transcript; the engine's resident-session, lease, slot and yield rules
  assume the engine owns the process; a terminal is a much larger grant
  than a chat. A plain remote shell into a workspace (no mirror, principal
  bound, recorded in access history) remains worthwhile on its own and is
  not part of this plan.
- **Import-on-exit** (convert a finished TUI session's JSONL into a
  read-only chat) is the cheap middle if a shell ever ships; same format
  dependency, none of the live-mirror machinery.
- **The harness on the user's machine with the sandbox as backend**: Claude
  Code's built-in tools run locally and it would move the enforcement
  boundary to the laptop.

Parity on Warden's surfaces keeps attribution, multi-user, grants,
previews, PR proposals and the live transcript first class, at the cost of
chasing Anthropic's UI: the target is a stable 80 %, not every release.

## Priority order (the plan)

Most important first. Each item lands on both surfaces unless marked.

1. **Typed tool cards and file diffs.** `claude.go` folds every `tool_use`
   into one generic `commandExecution` card ("name: input"). Emit typed
   items instead — Bash (command + output), Read/Grep/Glob (path, hits),
   Edit/Write/MultiEdit as `fileChange` with a diff (the web's `DiffView`
   already renders Codex's), WebFetch/WebSearch, Warden MCP tools — plus
   fold long output with an expand control. This is what lets anyone see
   what the agent did; nothing else matters until it is right.
2. **Subagents and background tasks.** Frames carry `parent_tool_use_id`;
   nest a subagent's activity under its Agent card with progress and an
   expandable child transcript. Background Bash / Monitor and task
   notifications as cards rather than loose messages. Todo list (TodoWrite)
   as a panel.
3. **Permission model.** Deny with a reason the agent sees; "allow always"
   (Warden persists a rule per chat or workspace and answers automatically;
   the SDK response also carries `updatedPermissions`); permission modes
   default / acceptEdits / plan (and bypass, if policy allows it) through
   `set_permission_mode`, Shift-Tab in the TUI, a selector on the web.
   Answering every prompt by hand is the biggest friction of driving Claude
   through a proxy UI.
4. **Plan mode.** Enter and leave plan mode; the plan file; ExitPlanMode as
   an approval with "auto-accept edits". Builds on 3.
5. **Slash-command pass-through.** Send `/cmd args` as the user message so
   the CLI expands built-ins (`/compact`, `/init`, `/review`,
   `/security-review`, `/pr-comments`) and the workspace's own commands and
   skills; list them in the `/` menu from `system/init`'s `slash_commands`.
   Cheap, unlocks a lot.
6. **TUI catch-up** (TUI only: what the web already has). `@path`
   completion from the `paths` op, a real `/` menu, attachments (`/attach`),
   diff rendering, output folding, export, rename/archive, the Claude
   keyboard set (Ctrl-C/D, Esc, Shift-Tab, Ctrl-O, Ctrl-T, Ctrl-B, Ctrl-R,
   Ctrl-L, Ctrl-U/K/W, Tab). Vim mode last.
7. **Workspace `.claude/` loading — verify and decide.** Warden launches
   with `--setting-sources=` and `--strict-mcp-config`. Check what the
   pinned CLI still loads from the workspace (commands, skills, agents,
   rules, `CLAUDE.md` imports) and decide policy for hooks (sandboxed, so
   probably allow), project MCP servers (stdio in-sandbox yes; network ones
   through the gateway) and plugins (fetch through the gateway).
8. **Compaction and context.** `/compact [instructions]`, the
   `compact_boundary` marker in the transcript, a context-left indicator and
   auto-compact warning from `result` usage against the model's window.
   Resident sessions make long conversations the norm.
9. **Mid-session model, effort, thinking.** `set_model`,
   `set_max_thinking_tokens`, effort level, thinking on/off; fast mode and
   the 1M variant behind policy (cost).
10. **Queueing and conversation rewind.** Queue a message while a turn
    runs and edit it before it sends; Esc-Esc / edit-and-resend with correct
    semantics on a resident session (fork the session or resume before the
    edited message).
11. **Checkpoints and session diff.** `/rewind` to a message restoring
    code, conversation or both — the SDK's file checkpointing
    (`rewind_files`) if the pinned CLI has it, else Warden-native via a
    sandbox snapshot/fork, which is stronger. A whole-session diff view
    (runner `git diff`, web `DiffView`).
12. **Composer polish.** Prompt history and Ctrl-R, long paste collapsed to
    "[Pasted text #N]", `!` to run a shell command in the workspace
    (attributed to the person; policy), `#` to append to `CLAUDE.md`.
13. **Per-user instructions and memory.** A user-level `CLAUDE.md` and
    auto-memory keyed by principal rather than by sandbox home; needs the
    multi-user model.
14. **Project MCP with remote OAuth, plugins.** The browser step through
    the owner (the GitHub device-flow pattern), `/mcp` status, MCP resources
    as `@server:resource`, MCP prompts as `/server:prompt`, plugin install.
15. **Long tail.** Fork a session, `/btw`, prompt suggestions, output
    styles, status line and terminal title, desktop notifications, `/cost`
    and `/context` breakdowns, share links (see the chat-sharing plan).

Out of scope: `/login`, `/logout`, `/upgrade`, `/doctor`, `/config`,
`/theme`, `/terminal-setup`, `/bug`, `/release-notes`, the auto-updater,
`--continue` across local directories, `/install-github-app`, Bedrock and
Vertex wiring, Claude's own bash sandbox, IDE integration, Claude in
Chrome, cloud sessions, Remote Control and teleport — process-local, or
replaced by Warden.

## Inventory

Status per surface: ✅ have · ◐ partial · ✗ missing · — not applicable.
"policy" marks a Warden decision rather than work.

### Transcript rendering

| Feature | Web | TUI | Notes |
|---|---|---|---|
| Streaming assistant text, markdown, highlighting | ✅ | ✅ | `stream_event` deltas |
| Thinking: collapsed, expandable, duration | ✅ | ✅ | text withheld by the CLI in `-p` mode |
| Spinner verbs and elapsed time | ◐ | ✅ | web has the status line |
| Typed tool cards (Bash, Read, Grep, Glob, Edit, Write, WebFetch…) | ✅ | ✅ | item 1: `agent/claude_tools.go`, `Entry.Tool`, `ToolCard.tsx`, `tui/render.go` |
| Edit/Write/MultiEdit as diffs | ✅ | ✅ | item 1: the CLI's `structuredPatch` hunks with line numbers |
| Output folding ("+N lines, expand") | ✅ | ✅ | item 1: 12 lines on the web, 8 in the TUI (Tab) |
| Subagent nesting, child transcript | ✅ | ✅ | item 2: `Entry.ParentID`, collapsed under the Agent card |
| Background task cards, task notifications | ✅ | ✅ | item 2: `Tool.Background`, `TaskOutput` lands the output |
| Todo panel (TodoWrite) | ◐ | ◐ | item 2: one card updated in place; the pinned CLI offers no todo tool |
| Compaction boundary marker | ✅ | ✅ | item 8: `compaction` entry (running → divider with trigger, counts, summary) |
| Context-left indicator, auto-compact warning | ✅ | ✅ | item 8: `conversation.context` {used, window, threshold} |
| Per-turn tokens, cost, duration | ✅ | ◐ | `TurnStats.tsx`; TUI elapsed only |
| Session cost total | ◐ | ✗ | |
| Inline images | ✅ | — | TUI: path + `/open` |
| Mermaid, math, links, path links | ✅ | — | |
| Search in transcript / across chats | ✅ | ◐ | TUI `/find` in chat |
| Copy message, export | ✅ | ◐ | TUI `/copy`; export ✗ |
| Unread divider, jump to bottom | ✅ | — | |
| Startup stages | ✅ | ✅ | Warden-only |

### Composer and input

| Feature | Web | TUI | Notes |
|---|---|---|---|
| Multi-line editing, newline chord | ✅ | ✅ | |
| Prompt history, Ctrl-R search | ✅ | ✅ | item 12 (web: from the transcript), item 6 (TUI) |
| `@path` completion | ✅ | ✗ | `paths` op |
| `@server:resource` | ✗ | ✗ | needs MCP resources |
| `/` menu with fuzzy match | ✅ (4) | ◐ typed | |
| Long paste collapsed | ✅ | ✅ | item 12: `[Pasted text #N — M lines]`, sent in full |
| Image paste, drop, picker | ✅ | ✗ | |
| File attachments into the workspace | ✅ | ✗ | |
| Queue a message during a turn | ◐ | ◐ | the CLI queues it (item 7); Warden's engine serialises turns itself; Codex steers |
| Edit a queued message | ✗ | ✗ | |
| Esc to interrupt | ✅ | ✅ | `turn/interrupt` |
| Esc-Esc / edit-and-resend | ◐ | ✗ | resident-session semantics to check |
| `!` shell command | ✅ | ✅ | item 12: by the person, transcript-only, `chats/{id}/exec` |
| `#` append to `CLAUDE.md` | ✅ | ✅ | item 12: `chats/{id}/memory`; read by the agent only once item 7's flag change lands |
| Prompt suggestions | ✗ | ✗ | |
| Vim mode | — | ✗ | |

### Turn control, permissions, plan mode

| Feature | Web | TUI | Notes |
|---|---|---|---|
| Tool permission prompt | ✅ | ✅ | `can_use_tool` → approvals; items 3–4: the call as its card (command, diff) in `ask` mode |
| Deny with a message | ✅ | ✅ | items 3–4: in the CLI's own rejection wording |
| Allow always (session / workspace rule) | ✅ | ✅ | items 3–4: Warden's rule per chat (`Chat.Allowed`); workspace-wide rules ✗ |
| Permission modes | ✅ | ✅ | items 3–4: auto / ask / plan per chat, selector and `/mode`, Shift-Tab; bypass never offered; policy per role open |
| Plan mode, ExitPlanMode approval | ✅ | ✅ | items 3–4: plan card, feedback, approve into auto or ask |
| AskUserQuestion | ✅ | ✅ | |
| Permission rules editor | ✗ | ✗ | Warden-owned rules at launch |
| Additional directories | — | — | sandbox is the boundary |
| Interrupt vs stop the sandbox | ✅ | ✅ | Warden-only |

### Model and session settings

| Feature | Web | TUI | Notes |
|---|---|---|---|
| Model choice | ✅ | ✅ | fixed per chat |
| Change model mid-session | ✗ | ✗ | `set_model` |
| Effort level | ✗ | ✗ | |
| Thinking on/off, budget | ✗ | ✗ | `set_max_thinking_tokens` |
| Fast mode, 1M context | ✗ | ✗ | policy |
| Output style | ✗ | ✗ | |
| Status line, terminal title | — | ✗ | |
| `/cost`, `/context`, `/usage` | ◐ | ✗ | plan limits n/a behind the gateway |

### Session lifecycle

| Feature | Web | TUI | Notes |
|---|---|---|---|
| New, rename, archive, delete | ✅ | ◐ | TUI `/new` only |
| Resume between turns | ✅ | ✅ | resident sessions |
| `/clear` | ✅ | ✅ | new chat |
| `/compact`, auto-compact | ✅ | ✅ | item 8: passthrough from the CLI's list; both triggers as dividers |
| Fork a session | ✗ | ✗ | pairs with sandbox fork |
| Auto titles | ◐ | ◐ | verify |
| Session picker | ✅ | ✅ | |
| Export | ✅ | ✗ | |
| Share link | ◐ | — | chat-sharing plan |
| `/btw` | ✗ | ✗ | |
| Several sessions in one workspace | ◐ | ◐ | sibling chats; sandbox fork |

### Files, checkpoints, git

| Feature | Web | TUI | Notes |
|---|---|---|---|
| `/rewind` (code, conversation, both) | ✗ | ✗ | `rewind_files` or sandbox snapshot |
| Whole-session diff | ✗ | ✗ | |
| Open / view a file | ✅ | ✅ | |
| Read renders images, PDFs, notebooks | ✅ | — | |
| Commit attribution | ✅ | ✅ | the CLI's |
| PR creation | ✅ | ✅ | reviewed-push proposals |
| `/security-review`, `/init`, `/code-review` (skill) | ✅ | ✗ | pass-through (item 5); `/review` and `/pr-comments` are not in CLI 2.1.272 |

### Slash commands, skills, agents, plugins

| Feature | Web | TUI | Notes |
|---|---|---|---|
| Built-in commands passed through | ✅ | ✗ | `system/init` lists them; `chat.commands` (item 5) |
| Project commands and skills | ◐ | ◐ | listed and expanded only once the launch loads `project` sources (item 7); `/name` reaches the CLI unchanged |
| Custom subagents | ✗ | ✗ | `.claude/agents` not loaded under `--setting-sources=` (item 7); `/agents` ✗ |
| Plugins | ✗ | ✗ | policy |
| MCP prompts as commands | ✗ | ✗ | |
| Output styles, status line, keybindings | ✗ | ✗ | |

### Memory and instructions

| Feature | Web | TUI | Notes |
|---|---|---|---|
| Project `CLAUDE.md`, imports, rules | ✗ | ✗ | not loaded under `--setting-sources=` (item 7); needs `project` |
| User-level `CLAUDE.md` | ✗ | ✗ | policy: per principal |
| Auto-memory | ◐ | ◐ | per sandbox home, not per principal |
| `/memory` editor | ✗ | ✗ | |
| Warden's appended system prompt | ✅ | ✅ | |

### MCP, hooks, integrations

| Feature | Web | TUI | Notes |
|---|---|---|---|
| Warden MCP tools | ✅ | ✅ | in-process `sdk` server |
| Project MCP servers | ✗ | ✗ | `--strict-mcp-config`; policy |
| Remote MCP OAuth | ✗ | ✗ | through the owner |
| `/mcp` status, resources, tool search | ✗ | ✗ | `mcp_status` |
| Hooks | ✗ | ✗ | `--setting-sources=`; policy |
| Notifications | ✗ | ✗ | |
| IDE, Chrome, computer use, cloud sessions | — | — | out of scope |

## Decisions needed

Recommendations from the item 7 probe (evidence under "Item 7" below);
the launch flags in `sandbox/runtime.go` `AgentCommand` are unchanged
until the owner confirms.

- **Hooks from the workspace's `.claude/settings*.json`.** Recommend
  `--setting-sources=project` plus `--settings '{"disableAllHooks":true}'`
  as the first step: it loads `CLAUDE.md`, its imports, `.claude/rules`,
  commands, skills and agents — none of which load today — and runs no
  hook. Enabling hooks is then one setting, on the owner's say-so: they
  run as the sandbox user, so the guest boundary holds, but they can
  spend tokens (`UserPromptSubmit` prompts) and act on every turn without
  a card in the transcript; with `includeHookEvents`-style frames
  (`hook_started`/`hook_progress`/`hook_response` exist in this CLI) they
  could be shown. `settings.local.json` needs `local`, which should stay
  off (it is the developer's own file, not the repository's).
- **Project MCP servers.** Keep `--strict-mcp-config` for now: `.mcp.json`
  never appears under any `--setting-sources` value, so the flag alone
  decides. When Warden wants them: stdio servers in-sandbox are safe by
  the same argument as hooks; HTTP/SSE servers go through the gateway's
  destination policy like any other egress (the CLI's proxy env is
  honoured); plugins are a later item (14) and would be fetched through
  the gateway as well. The CLI can also be told the servers at runtime
  (`mcp_set_servers`, `mcp_toggle` control requests) if Warden prefers to
  own the list.
- **Permission modes per role.** Not probed here (item 3). `permissionMode`
  is in `system/init` and `set_permission_mode` is a control request in
  this CLI, so a selector needs no relaunch.
- **`!` shell commands.** Item 12 runs them as the sandbox's agent user
  through the runner, attributed to the requester and open to anyone
  the edge admits to the chat (who could already have the agent run
  anything); an owner-only rule at the edge (`ownerOnly`) is one line if
  wanted.
- **Per-principal instructions and memory.** The CLI reports
  `memory_paths.auto` = `~/.claude/projects/<cwd>/memory/` in the sandbox
  home, so auto-memory is per sandbox; a per-principal layout needs
  Warden to set `HOME`/`CLAUDE_CONFIG_DIR` or to inject the user's
  `CLAUDE.md` — item 13.

## To verify on the pinned CLI

Answered 2026-09-17 against CLI 2.1.272 (see "Item 7" below for how):

- **What `--setting-sources=` still loads from the workspace:** nothing.
  With Warden's flags the workspace's `.claude/commands`, `.claude/skills`,
  `.claude/agents`, `.claude/rules`, `CLAUDE.md` and its `@imports`, and
  both `settings*.json` files are all ignored; `system/init` lists only the
  CLI's bundled skills and built-in commands, and the model answers that it
  has no project instructions. `--setting-sources=project` loads all of
  them (and runs `settings.json` hooks); `local` adds `settings.local.json`.
- **`rewind_files` / file checkpointing:** present but gated. The binary
  implements the `rewind_files` control request (`user_message_id`,
  `dry_run` → `canRewind`, `filesChanged`, …); in `-p` mode checkpoints are
  recorded only with `CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING` set in the
  environment (the SDK's `enableFileCheckpointing` option sets it) and
  `CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING` unset. Warden's launch sets
  neither, so nothing is recorded today. Not exercised live (item 11 will
  set the variable and try it); `rewind_conversation`, `fork_conversation`
  and `get_workspace_diff` requests also exist.
- **A user message during a running turn:** queued. Sent while the first
  turn's API request was in flight, the second message ran as its own turn
  after the first completed (two `result` frames, `result_index` 0 and 1,
  the first answer intact). Messages that arrive before the first API
  request goes out are merged into one turn (the model saw both and
  answered the last). `system/init` is emitted again for every turn.
  `capabilities` lists `interrupt_cancel_queued_v1`, so an interrupt also
  drops what is queued. Warden's engine serialises turns itself, so this
  only matters for item 10's queue-and-edit.
- **`system/init` contents:** `slash_commands` (bundled skills + built-ins:
  `agents auto-mode-setup autocompact clear color compact config
  output-style context effort fast heapdump init mcp model
  __remote-workflow workflow-launch-exec reload-plugins reload-skills
  rename security-review usage insights recap goal list-agents
  team-onboarding`; no `review` or `pr-comments` in this version),
  `terminal_slash_commands` (`doctor color reload-plugins`: terminal-only,
  to hide), `skills`, `agents` (`claude Explore general-purpose Plan
  statusline-setup` built in), `mcp_servers` (`warden` with its status),
  `tools` (24: `Task AskUserQuestion Bash CronCreate CronDelete CronList
  Edit EnterPlanMode EnterWorktree ExitPlanMode ExitWorktree ListAgents
  NotebookEdit Read ReportFindings ScheduleWakeup SendMessage Skill
  TaskOutput TaskStop WebFetch WebSearch Workflow Write` — no Grep, Glob,
  MultiEdit or TodoWrite in this version, relevant to item 1),
  `model`, `permissionMode`, `output_style`, `plugins`, `memory_paths`,
  `capabilities` (`interrupt_receipt_v1 interrupt_cancel_queued_v1
  msg_lifecycle_v1`), `fast_mode_state`, `apiKeySource`,
  `claude_code_version`, `cwd`, `session_id`.
- **What `/compact` returns in stream-json mode:** `system/status`
  `compacting`, then a fresh `system/init` (same `session_id`), a
  `system/compact_boundary` frame with `compact_metadata` (`trigger:
  manual`, `pre_tokens`, `post_tokens`, `cumulative_dropped_tokens`,
  `duration_ms`), two `user` frames with string content (the summary,
  marked `isReplay`/`isSynthetic`, and `<local-command-stdout>Compacted
  </local-command-stdout>`), and a `result` with an empty `result`,
  `num_turns: 0` and the compaction's cost in `total_cost_usd`. About 28 s
  for a two-turn conversation. Also observed: any first word of the form
  `/name` is taken as a command; an unknown one yields a synthetic
  `assistant` message `Unknown command: /name` (`model: <synthetic>`) and a
  success `result` without a model call, while `/etc/hosts …` or a lone
  `/` reach the model as text.

## Working method

- One worktree per item from `origin/main` (`feat/parity-<n>-<topic>`),
  landed with the merge-to-main procedure (fast-forward only, full Go and
  web verification on the merged tree). Items whose files do not overlap
  run in parallel; the adapter's `tool_use` handling (item 1) lands before
  the items that build on it.
- Live testing never shares `~/.warden`: each item runs its own cloned
  Warden home (`~/.warden-p<n>`, own SBX namespace and daemon, own ports;
  the workspace's `.local/clone-warden-home.sh`), deployed with
  `WARDEN_HOME=<home>/release scripts/deploy-local.sh --no-restart`.
- Each item ticks its box below with the merge sha and a line on how it
  was verified.

## Progress

- [x] Design discussion, inventory and priority order (this document).
- [x] 1 Typed tool cards and diffs — merged to main 5715a02 (2026-09-17); verified as the Item 1 section says.
- [x] 2 Subagents and background tasks — merged to main 38daa78 (2026-09-17); verified as the Item 2 section says.
- [x] 3 Permission model — merged to main 5f38f23 (2026-09-17); verified as the Items 3 and 4 section says.
- [x] 4 Plan mode — merged to main 5f38f23 (2026-09-17); verified as the Items 3 and 4 section says.
- [x] 5 Slash-command pass-through — merged to main 32138ea (2026-09-17); verified as the Item 5 section says; TUI `/` menu from `chat.commands` left for a follow-up.
- [x] 6 TUI catch-up — merged to main c60d938 (2026-09-17); verified as the Item 6 section says.
- [x] 7 Workspace `.claude/` loading — verified on CLI 2.1.272, merged to main 32138ea (2026-09-17); the launch-flag change (`--setting-sources=project` + `disableAllHooks`) is recommended under "Decisions needed", not made.
- [x] 8 Compaction and context — merged to main e84a7bc (2026-09-17); verified as the Item 8 section says.
- [ ] 9 Mid-session model, effort, thinking.
- [ ] 10 Queueing and rewind.
- [ ] 11 Checkpoints and session diff.
- [x] 12 Composer polish — merged to main 981ef68 (2026-09-17); verified as the Item 12 section says.
- [ ] 13 Per-user instructions and memory.
- [ ] 14 Project MCP, OAuth, plugins.
- [ ] 15 Long tail.

### Item 13: per-user instructions and memory

Branch `feat/parity-13-user-memory`, worktree
`.local/warden-parity-13-user-memory`, from main a3b0a00, fast-forwarded to
5114d71 (item 12) before the first code (2026-09-17). Started 2026-09-17.

What exists: a chat's entries carry `Sender` (the principal the edge
identified: `owner`, or a Google `sub` with email and name); a chat has no
record of who created it. Claude's only system prompt is the
`--append-system-prompt` text in `sandbox/runtime.go` `AgentCommand`
(the adapter ignores the engine's `developerInstructions`, which Codex
takes at `thread/start`). `--setting-sources=` means the CLI reads no
`CLAUDE.md`, rules or auto-memory from the workspace (item 7); it still
reports `memory_paths.auto` in `system/init`. Item 12's `#note` appends
to `CLAUDE.md` through runner op `memory-append` and `POST
chats/{id}/memory`.

Steps:

1. Per-principal instructions in the chat store (`State.Instructions`,
   keyed by principal), `GET`/`POST me/instructions`; web "Instructions…"
   in the sidebar footer (a markdown textarea dialog), TUI
   `/instructions` shows and `/instructions edit` loads them into the
   composer, Enter saves.
2. Delivery: at a Claude launch the runner appends, after Warden's own
   prompt, one block per participant — "Instructions from <name>:" + text
   — for the chat's creator and every sender so far (`Request.
   Instructions` on the `stream` op → `RunSpec.Instructions` →
   `claudeSystemPrompt`); Codex gets the same blocks appended to
   `developerInstructions`. A person whose block the live session has not
   seen (a late joiner, or instructions changed since) gets it once as a
   prefix on their next message, both providers.
3. Memory view: `GET chats/{id}/memory` lists the workspace's `CLAUDE.md`,
   `CLAUDE.local.md`, `AGENTS.md`, `.claude/CLAUDE.md`, `.claude/rules/**.md`
   and the CLI's auto-memory directory (`memory_paths.auto` from
   `system/init`, kept on `Chat.Session`, validated by the runner; else
   derived) with contents; `POST chats/{id}/memory/write` writes one file
   (runner op `memory-write`: staged and copied in like an attachment,
   placed by a descriptor-relative script, paths limited to those
   locations). Web: a "Memory" section in the workspace panel with an
   editor dialog; TUI `/memory`, `/memory FILE`, `/memory edit FILE`.
4. Attribution: every write leaves a `notice` entry "<name> edited
   CLAUDE.md" with `Sender`.
5. Tests, feature map, live check on a cloned home, merge.

Decisions:

1. Instructions live in Warden's store, keyed by principal, never in the
   sandbox: a workspace is shared by chats and people, and the sandbox
   home is the agent's; the store is where the principal already exists.
   Display name for the block: the person's name, else email, else "the
   owner" for the owner principal.
2. Delivery is plain text in the system prompt (and a message prefix),
   never a policy change; a block is quoted as the person's, so
   instructions that read like commands to Warden stay text.
3. The instructions ride the `stream` request as data (`AgentCommand`'s
   argument list; the drivers never go through a shell); one person's
   text is capped at 16 KiB so the argument list stays small.
4. `GET`/`POST` rather than `PUT`: the service answers `GET` and `POST`
   only and the web client speaks those two.
5. The memory listing shows what exists even though the launch reads
   none of it (item 7); each file carries `read: true|false` so the
   surfaces can say so, and the auto-memory directory is listed only when
   it exists.
6. `#` (item 12) is left as it is; the write route is separate
   (`memory/write`) so append and replace do not share one verb.

Progress: started 2026-09-17.

### Item 12: composer polish

Branch `feat/parity-12-composer`, worktree `.local/warden-parity-12-composer`,
from main d74ac80 (2026-09-17). Started 2026-09-17.

Steps:

1. Web prompt history: Up/Down at the draft's first/last line recall this
   chat's earlier prompts (the transcript's user entries by this
   principal, newest first; the draft is kept), Ctrl-R searches them
   (`history.ts`).
2. Long paste: a paste over 8 lines or 1000 characters becomes a
   `[Pasted text #N — M lines]` placeholder in the text and a chip above
   it (hover previews, click opens, remove drops both); the message sends
   with the full text; several per draft; persisted with the draft
   (`paste.ts`, `drafts.ts`). TUI: the same placeholder from bracketed
   paste, expanded on Enter.
3. `!cmd`: the rest runs as a shell command in the workspace by the
   person — `POST chats/{id}/exec` → runner op `exec` (both drivers,
   cwd = workspace, 60 s, output capped at 30k) — recorded as a command
   card with `Sender`, transcript-only, with "Send to agent". Web and TUI.
4. `#note`: appended as a bullet to the workspace's `CLAUDE.md` — `POST
   chats/{id}/memory` → runner op `memory-append` — with a system line
   "Added to CLAUDE.md" and the item 7 hint. Web and TUI.
5. Tests, feature map, live check on a cloned home, merge.

Decisions:

1. A `!` command's entry is transcript-only: the agent never sees it
   unless the person quotes it into a message ("Send to agent" puts the
   command and its output into the draft as a fenced block). Reason: the
   command ran outside the agent's turn, by a person; feeding it to the
   model silently would make the agent act on output it did not ask for,
   and a resident session cannot take an out-of-band user message without
   it becoming a turn.
2. Attribution: the entry carries `Sender` (the requester the edge
   identified, or the owner), the runner request carries the principal,
   and the chat service logs who ran what with the exit code.
3. The runner runs the command as the sandbox's agent user in a bash
   process group, kills the group at the timeout, and returns stdout and
   stderr merged with the exit code; the command is passed as data, never
   through a host shell. It runs outside the registry lock and on its own
   request slots, so a slow command blocks neither the turn nor the
   workspace panel.
4. The web's history comes from the transcript (this principal's user
   entries), not from local storage: every device sees the same list and
   nothing new is stored. The TUI keeps item 6's per-chat file.
5. The paste thresholds (8 lines or 1000 characters) and the placeholder
   text are the same on both surfaces; a placeholder deleted from the
   draft drops its paste, a placeholder typed by hand stays text.
6. "#" for Codex chats still writes CLAUDE.md (the note is for Claude);
   the system line says Codex reads AGENTS.md instead.

Verified 2026-09-17 on a cloned home (`~/.warden-p8`, build a19cc28, CLI
2.1.272): unit — `go test ./...` (`sandbox/exec_test.go` runs the guest
scripts locally: cwd, merged stderr, exit codes, the output tail, the
timeout killing the group, a background child not holding the answer,
CLAUDE.md created/appended/never through a symlink; the ops through the
fake guest; `chats/composer_test.go` both routes; `tui/tui_test.go`
`!`/`#`/placeholders), `pnpm test` (144: `history.test.ts`,
`paste.test.ts`, prefixes, grouping). Live through the API: `ls -la`,
`git status` in a workspace the agent had `git init`ed, `exit 7` with
stderr, two `#` notes then `cat CLAUDE.md` showing the bullets (with the
indented continuation line), a `!` command answering in 0 s while a 25 s
agent turn ran, and the agent's next reply confirming it never saw the
person's commands. In the browser: Up/Down walked three prompts and back
to the empty draft, Ctrl-R + "hello" + Enter recalled the first prompt, a
240-line paste became `[Pasted text #1 — 240 lines]` with its chip,
survived a reload, opened in the dialog and was sent in full (241 lines
in `GET state`; Claude answered "log line 137"), `!git status --short;
git log --oneline` from the composer (hint, terminal send icon, the "You"
card open with `?? CLAUDE.md`), "Send to agent" quoting it into the
draft, `#prefer small commits` → the system line and `!cat CLAUDE.md`
showing it, a `!` card landing while the agent's turn ran. TUI in a pty:
`!echo …; exit 4` → "you $ …" card with `exit 4` and the notices, `#`
→ the system line, a 14-line bracketed paste → the placeholder.

Left: the runner's `exec` has no per-role policy (see "Decisions
needed"); a `!` command's card is not searched by the transcript find
(activity entries never were); the TUI's paste placeholder is not a
chip (no preview), and a `!` command with a paste placeholder expands it
on the TUI too but the TUI shows no chip to inspect first.

Progress: started 2026-09-17; implemented and live-verified 2026-09-17
(a19cc28); merged to main 981ef68 (2026-09-17) after merging items 3, 4
and 8 in (append-append conflicts in the TUI and its tests, the
Conversation.tsx imports, both docs); the merged build re-checked on the
cloned home (a `!` against the stopped sandbox gives the clean refusal
card; Ctrl-R finds the pasted prompt; the mode selector sits beside the
composer's).

### Item 8: compaction and context

Branch `feat/parity-8-compaction`, worktree `.local/warden-parity-8-compaction`,
from main d648409 (2026-09-17).

What the pinned CLI (2.1.272) emits, probed on a cloned home (`~/.warden-p6`)
with a second CLI run inside the chat's sandbox under the resident process's
environment and Warden's launch flags (`scratchpad/probe.py`: one `user`
frame per turn, the next sent after the `result`), on a session that read
three 1500-line files (~171k tokens of context), then `/compact keep the
list of files read and their last words`, a question, `/compact`, and on a
fresh session that read seven such files:

- **Context length.** Every `assistant` frame (one per content block) and
  the `message_start` stream event carry the API call's `message.usage`:
  `input_tokens` (the uncached part, 2), `cache_creation_input_tokens`,
  `cache_read_input_tokens`. Their sum is the prompt size of that call —
  the context length: 40.6k → 109k → 171k over one turn's three calls.
  The `result`'s `usage` is **summed over the turn's calls** (142127
  cache-creation = 11788 + 68398 + 61941), so it cannot give the context;
  the last call's usage does.
- **Context window.** `result.modelUsage[<model>].contextWindow` reports
  it (200000 for `claude-sonnet-5` here; also `maxOutputTokens`,
  `canonicalModel`, running per-model totals and cost). The binary's model
  table agrees: 200k for haiku 4.5, sonnet 4.0/4.5, opus 4.0/4.1/4.5
  (`[1m]` suffix → 1M where `supports_1m_suffix`); native 1M for sonnet
  4.6, opus 4.6/4.7/4.8, opus 5, fable 5. The CLI's own auto-compact
  window can be set below the model's (`CLAUDE_CODE_AUTO_COMPACT_WINDOW`,
  `autoCompactWindow`); Warden does not set it.
- **`/compact` and `/compact <instructions>`** (both `trigger: manual`;
  the instructions shaped the summary, which kept the file list and last
  words): `system/status` `{status: "compacting"}` → after 11–23 s
  `system/status` `{status: null, compact_result: "success"}` →
  `system/init` (same `session_id`) → `system/compact_boundary` with
  `compact_metadata` `{trigger, pre_tokens: 171238, post_tokens: 2194,
  cumulative_dropped_tokens, duration_ms, preserved_segment,
  preserved_messages}` → a `user` frame with the summary as **string**
  content (`isSynthetic: true`, `isReplay: false`; "This session is being
  continued from a previous conversation that ran out of context. The
  summary below…", 4–6k chars) → a `user` frame `isReplay: true` with
  `<local-command-stdout>Compacted </local-command-stdout>` → `result`
  (`num_turns: 0`, empty `result`, all-zero `usage`, `total_cost_usd`
  grown by the compaction's own call, `duration_api_ms: 0`).
  `pre_tokens` is the whole context (system prompt and tools included:
  last call 170932 + its output); `post_tokens` is the summary alone —
  the next call's context was 41.1k (37.2k cache read of the fixed
  prefix + 3.9k summary).
- **Auto-compaction** (fresh session, seven files; the context reached
  171k, the seventh read pushed it past the threshold): mid-turn
  `status: "compacting"` → 24 s → `status: null, compact_result:
  "failed", compact_error: "API Error: …"` (the summary request was
  refused: an AUP classifier false positive on the random word lists) →
  a synthetic `assistant` frame (`model: "<synthetic>"`,
  `is_api_error_message: true`, text "Prompt is too long · automatic
  compaction failed: …") → `result` `is_error: true` with that text. The
  next turn retried before its API call: `status: "compacting"` (re-sent
  once after 30 s) → `compact_result: "success"` → `compact_boundary`
  `{trigger: "auto", pre_tokens: 184293, post_tokens: 2484}` → the
  summary as a `user` frame whose content is a **list** of text blocks
  (`isSynthetic: true`) → the turn's normal API call at 41.1k. No
  `system/init` between (it had come at the turn's start).
- Also: `system/status {status: "requesting"}` precedes every API call;
  `system/init` carries no window (`model`, `tools`, `slash_commands`,
  … as item 5 records).
- **`get_context_usage`** (a client control request, answered in about
  a second, between turns and mid-turn): `totalTokens` (the context as
  the CLI counts it — the same figure as the last call's usage, 40541
  both ways), `maxTokens` and `rawMaxTokens` (200000),
  `autocompactSource: "model-default"`, `percentage`, `categories`
  (`System prompt` 8516, `System tools` 27263, `Skills` 1941,
  `Messages`, `Autocompact buffer` 33000 with `kind: "buffer"`, `Free
  space`) and `gridRows` for the CLI's own `/context` picture. So the
  CLI compacts on its own at the window less 33k = 167k on the 200k
  models, which matches the auto-compaction seen at 184k–189k
  `pre_tokens` (the last read's output pushed it past).

Design:

1. Adapter (`claude.go`, the `system` non-init subtypes, the synthetic
   `user` frame and the `assistant`/`result` usage): a compaction is a
   transcript item of type `compaction` — `item/started` at `status:
   compacting` (so the 10–30 s show as "Compacting context…" in the
   transcript and the status line), `item/completed` at the boundary with
   `trigger`, `preTokens`, `postTokens`, and again with `summary` when the
   synthetic user frame follows; `compact_result: failed` completes it as
   `failed` with the error (the CLI's own error result still ends the
   turn). The context is a `thread/context/updated` notification `{used,
   window, threshold, model}`: as the turn runs, `used` from each
   `assistant` frame's usage (input + cache creation + cache read; the
   conversation's own calls, not a subagent's) with `window` from
   `result.modelUsage` once seen (a table by model id before that: 1M
   for the `[1m]` suffix and the native-1M ids above, 200k otherwise)
   and, at a boundary, `post_tokens` + the smallest context the process
   has seen (the fixed prefix) as the estimate; then the CLI's own
   account, `get_context_usage` sent after every `turn/completed` and
   after every boundary (from a goroutine behind a mutex on the CLI's
   stdin, so a slow reader never stalls the adapter), whose answer gives
   `used` exactly, `window`, and `threshold` = `maxTokens` − the buffer
   category. One boundary path: item 5's `thread/compacted` → system
   line (which its plan marked as item 8's to replace) is gone.
2. Conversation: entry role `compaction` with `Compaction{Trigger,
   PreTokens, PostTokens, Status, Error}` and the summary in `Detail`;
   `Conversation.Context{Used, Window, Threshold, Model}` kept by the
   engine from the notification (in `GET state` / `events` as
   `conversation.context`). Codex reports no window today; the field
   stays nil.
3. Web: the divider "Context compacted · manual · 171k → 2.2k tokens"
   with the summary behind a disclosure, "Compacting context…" while it
   runs (the status says so too), the error when it failed; a context
   meter beside the model in the composer footer ("42k / 200k" with a
   tick at the threshold; amber from 80 % of the way to the threshold,
   red from 95 %, the title naming the threshold and what happens); a
   `/compact` turn's footer shows its duration and cost, not "0 tokens".
   `/compact` comes from the CLI's list in the `/` menu (item 5). TUI:
   `ctx 42k/200k (21%)` in the status line (yellow/red on the same way
   to the threshold), the divider (summary on Tab), `/compact
   [INSTRUCTIONS]` in the menu from the chat's reported list (the
   provider default until it reports), sent as text; `warden chat send
   --wait` prints the divider line.

Verified (2026-09-17): `go vet`, `gofmt -l`, `go test ./...` (the
`kube` framing test flakes under the full run and passes alone, as
noted before), `pnpm build`, `pnpm test` (139 tests; `context.test.ts`
new); live on a cloned home (`~/.warden-p6`, build e664770) with a
Claude chat: three 1500-line files generated and read (the context grew
to 189k and the CLI compacted on its own mid-turn — divider "Context
compacted · automatic · 189k → 25k tokens", summary captured, the meter
at 100k / 200k afterwards; `GET state` `context {used: 99747, window:
200000}` while the turn's summed usage said 593k); `/compact keep the
list of files and their last words` from the web composer (picked from
the `/` menu) — "Compacting context…" in amber with the turn timer and
the status "Compacting context" for 27 s, then "Context compacted ·
manual · 100k → 2.7k tokens" with the footer "27s · $0.16" and the meter
down to 49k / 200k; the follow-up "which files…" answered correctly from
the compacted session (context 47.3k, the estimate had said 48.7k);
`/compact` through `warden chat send` (a third divider, 47k → 2.7k);
after the merge, `context.threshold: 167000` from `get_context_usage`,
the meter's tick at 83.5 % with "at about 167,000 tokens" in its title,
and the TUI under a pty: status line `… $0.03  ctx 49k/200k (25%)`, the
three dividers, `/comp` → `/compact [INSTRUCTIONS]` → Tab.

### Item 1: typed tool cards and diffs

Branch `feat/parity-1-tool-cards`, worktree `.local/warden-parity-1-tool-cards`,
from main 5ff4767 (2026-09-17).

What the CLI gives: every `tool_result` frame carries, beside the text the
model reads, a structured `tool_use_result` (probed on the pinned 2.1.27x
with `claude -p --output-format stream-json`): Edit's `structuredPatch` is
the CLI's own hunks with line numbers; Write says `create` or `update` and
patches an existing file against its old content; Bash separates stdout
and stderr; Read gives the file's line range; a tool's error is a string
and the text form is wrapped in `<tool_use_error>`. The pinned CLI offers
no Grep, Glob or TodoWrite tools (the model uses Bash); their mapping is
in place and unit-tested for a CLI that has them.

Decisions:

1. The adapter emits Codex-shaped items where Codex has the kind
   (`commandExecution`, `fileChange`, `mcpToolCall`, `webSearch`) and a new
   generic `toolCall` (`tool`, `kind`, `title`, `input`, `output`, `paths`,
   `query`, `status`) for the rest, so the conversation layer stays one
   mapping for both providers.
2. `Entry.Tool` is additive (`kind`, `name`, `server`, `status`,
   `description`, `paths`, `query`, `input`). With it set, `Detail` is the
   output or diff alone; entries recorded before it keep their status-line
   `Detail` and the generic rendering, so `GET state` stays compatible and
   old transcripts render as before.
3. An Edit's diff is written twice: at the call's start from `old_string`
   and `new_string` as a hunk without an `@@` header (the line is not
   known, and a wrong number would be worse than none; the surfaces colour
   such a hunk by prefix without numbers), then replaced by the CLI's
   numbered hunks from `structuredPatch` at the result. A Write is its
   content as an added file (`new file` once the CLI says `create`; the
   CLI's patch when it replaced an existing file). MultiEdit and
   NotebookEdit stay headerless.
4. Paths inside `/home/agent/workspace` read relative to it on both
   surfaces; the web's read card links the path to the file route, which
   accepts either form.
5. Folding: the web shows the first 12 lines with a "+N lines" control
   (the last 12 while a command still streams); the TUI shows 8 (the last
   ones of a command's output, the first of anything else) until Tab.
   Reads and searches collapse to one line with their line or hit count.
6. Kept out of item 1: a subagent's own tool calls (`parent_tool_use_id`)
   render as sibling cards until item 2 nests them; TodoWrite is a generic
   card until item 2's panel.

Verified: `go vet`, `gofmt -l`, `go test ./...`, `pnpm build`, `pnpm test`
(120 tests, `tools.test.ts` new); live on a cloned home (`~/.warden-p1`)
with a Claude chat that ran Bash (with a description and a failing
command), Read (and a failing read), grep, Edit (numbered hunk from
`structuredPatch`), Write (`new file`), a Warden MCP tool, WebFetch
(refused by policy: a failed fetch card) and an Explore subagent —
inspected through `GET state`, the web UI (diffs with line numbers,
`+N lines` fold, failed badges, read path link) and the TUI in a pty
(collapsed and Tab-expanded); a Codex chat's command renders through the
same model (its file edit could not run: the account's Codex usage limit
was exhausted; the `fileChange` mapping is unit-tested).

### Item 2: subagents, background tasks, todo list

Branch `feat/parity-2-subagents`, worktree `.local/warden-parity-2-subagents`,
from main 8f0b720 (2026-09-17).

What the CLI gives (probed on 2.1.275 in Warden's exact launch mode,
`-p --input-format stream-json --include-partial-messages`; the guest's
2.1.272 to be confirmed live):

- **Subagents.** The tool is `Agent` (`system/init` lists it as `Task`).
  A subagent's frames are complete `assistant` and `user` messages with
  `parent_tool_use_id` set to the Agent call's id; no `stream_event`
  carries a parent, so a subagent's text never streams. Two shapes:
  - *Foreground* (`run_in_background: false`, `task_started` with
    `is_backgrounded: false`): a `user` text frame with the prompt, the
    child's tool calls and results, then the parent's `tool_result` whose
    content is the child's final text and whose `tool_use_result` carries
    `totalDurationMs`, `totalTokens`, `totalToolUseCount`, `usage`,
    `agentType`. The child's own final text and thinking are not emitted.
  - *Async* (the default on 2.1.275 when `run_in_background` is unset:
    `subagent_stats.requested.unset`, `started_in_background: 1`): the
    parent's `tool_result` comes back at once ("Async agent launched",
    `tool_use_result.isAsync: true`, `status: "async_launched"`,
    `agentId`), the parent goes on and its turn ends (`result`) while the
    child keeps sending frames; the child's final text arrives as a child
    `assistant` text frame; then `system/task_notification` (`tool_use_id`,
    `status`, `summary` = the child's final text, `usage`) and the CLI
    resumes the model by itself: a new `system/init`, a turn with no user
    message, a second `result` with `origin: {kind: "task-notification"}`.
- **Background commands.** Bash with `run_in_background: true`:
  `system/task_started` (`task_id`, `tool_use_id`, `description`,
  `task_type: local_bash`), the `tool_result` says "Command running in
  background with ID …" with `tool_use_result.backgroundTaskId`; on exit,
  `system/task_updated` (`patch.status`, `end_time`) and
  `system/task_notification` (`status: completed|failed`, `summary`
  "Background command … completed (exit code 0)" / "failed with exit code
  3", `output_file` inside the sandbox). The output itself only reaches the
  transcript when the model reads it: `TaskOutput {task_id, block,
  timeout}` returns `tool_use_result.task {task_id, task_type, status,
  description, output, exitCode}`. A notification after the turn ended
  makes the CLI resume the model as above. Also seen: `task_progress` for
  agents (`usage.total_tokens`, `tool_uses`, `duration_ms`,
  `last_tool_name`), `background_tasks_changed`, `task_summary`,
  `post_turn_summary`, `status`, `thinking_tokens` — all ignored.
- **Todo list.** 2.1.275 has no `TodoWrite`; the list is `TaskCreate
  {subject, description, activeForm}` → `{task: {id, subject}}`,
  `TaskUpdate {taskId, status: pending|in_progress|completed|deleted, …}` →
  `{statusChange: {from, to}}`, `TaskList {}` → `{tasks: [{id, subject,
  status, blockedBy}]}`, `TaskGet {taskId}`. `TodoWrite`'s documented
  shape (`{todos: [{content, status, activeForm}]}`) is mapped for a CLI
  that has it.

Decisions:

1. Nesting is one additive field, `Entry.ParentID`: the Agent call's
   entry id (its tool_use id) on every entry the subagent produced; ""
   at the top level. Nested subagents chain by the same rule. The adapter
   sets `parentId` on the items; `Upsert` copies it; `Hydrate` and
   `Finish` need nothing more. A child entry carries the turn its Agent
   call was made in, even when it arrives after that turn ended.
2. A subagent's final text is the Agent card's result (`Detail`): the
   parent's `tool_result` content in the foreground case, the
   notification's `summary` in the async case. `Entry.EndedAt` is set on
   the card when the subagent finished, so both surfaces show the elapsed
   time; the tool-call count is the children's.
3. `Tool.Background` marks a command or an agent the CLI runs in the
   background (from `run_in_background`, `backgroundTaskId`, or an async
   launch); such a card stays running past its `tool_result` (whose
   boilerplate is dropped) until the task's notification, which sets the
   status (completed, or failed by status or a non-zero exit code in the
   summary) and, for a command, the summary line as a placeholder output;
   a `TaskOutput` result replaces it with the real output and exit code.
   `TaskOutput`, `TaskStop` and `Monitor` are generic cards titled with
   the task's description.
4. A turn the CLI starts by itself (after a task notification) is a turn
   to Warden too: the adapter opens one (`turn/started`) at the first
   top-level frame after a `result`, and the engine's idle wait
   (`awaitMessage`) hands such a turn to the session loop, which drives
   it like any other (running status, Stop, turn record, usage) without
   sending a message.
5. The todo list is one entry per adapter process (`todoList` item,
   `Tool.Kind: "todo"`, the list as `Tool.Input.todos` in TodoWrite's
   shape and as text lines in `Detail`), updated in place by every
   `TodoWrite`/`TaskCreate`/`TaskUpdate`/`TaskList`/`TaskGet` result; the
   calls themselves get no card.
6. Web: child entries are dropped from the top-level list and rendered
   inside the Agent card (`nestEntries` in `transcript.ts`), collapsed
   behind "n steps · elapsed"; the card shows the prompt, the child
   transcript and the result. Turn footers, unread and jump counts skip
   children. TUI: children indent under the card, Tab expands them.
7. A `result` whose `origin.kind` is `task-notification` arriving while
   the turn Warden asked for still runs (seen by item 6: a foreground
   Bash still streaming) does not end that turn: the answer is a message
   in it and the turn ends with its own result. Only a turn the CLI
   started ends on such a result.

On the guest's 2.1.272 (live): the model sees `Agent`, `TaskOutput` and
`TaskStop` but no `Monitor`, `TodoWrite` or `TaskCreate/TaskUpdate/
TaskList/TaskGet`, so the todo card is unit-tested only there; the
subagent runs in the foreground unless asked for `run_in_background`; an
async subagent's notification after the turn made the CLI start a turn
of its own, exactly as on 2.1.275; `TaskOutput`'s structured result is the
same shape.

Verified: `go vet`, `gofmt -l`, `go test ./...`, `pnpm build`, `pnpm test`
(128 tests; `transcript.test.ts` and `tools.test.ts` extended, the CSS
brace test now covers `conversation.css`); live on a cloned home
(`~/.warden-p4`, build 97e22d6) with a Claude chat that ran a background
Bash (`run_in_background`), a foreground Explore subagent and
`TaskOutput` in one turn, then an async Explore subagent whose result
arrived after the turn: through `GET state` (child entries with
`parentID` and the parent's turn, the Agent card completed at the
notification with `endedAt` and the child's text, the background card
with `background: true` and the real output, the CLI-started turn with
its own record and usage, chat idle after), the web UI (the group's
cards with the `background` badge and "1 tool call · 4s", the Agent card
expanded to its prompt, the nested "Explore: 1 tool call, 1 message"
transcript with the child's command card and message, and the result;
the CLI-started turn's message with its stats line) and the TUI in a pty
(collapsed count line, Tab expanding the indented child command and the
`Explore ›` message before the result, `[background]` mark).

Left: a subagent's entries are not found by the transcript search or
counted in the export as nested; `task_progress` (the subagent's current
step) is not shown while it runs beyond the child cards themselves; the
todo card has no live test until the pinned CLI offers a todo tool.

Progress: started 2026-09-17; implemented and live-verified 2026-09-17
(97e22d6); merged to main 38daa78 (2026-09-17) after merging item 6's
TUI catch-up in (the plan document was the only conflict).

### Item 6: TUI catch-up

Branch `feat/parity-6-tui`, worktree `.local/warden-parity-6-tui`, from
main 5ff4767 (2026-09-17). TUI only; `render.go`'s entry rendering is item
1's and is left alone.

Steps:

1. Composer completion: `@path` from the `paths` route, a `/` menu with
   hints (fuzzy prefix match as `composer.ts`), Tab/Enter accept.
2. Attachments: `/attach PATH` uploads through `chats/{id}/attachments`,
   sent with the next message; `/attachments`, `/detach N`.
3. `/export [md|json] [all] [FILE]`, the same content as `export.ts`.
4. `/rename`, `/archive`, `/restore`, `/delete` (confirmed).
5. Keys: Ctrl-C (clear, twice quits), Ctrl-D (quit on empty), Esc
   (interrupt), Ctrl-O (verbose), Ctrl-L (redraw), Ctrl-U/K/W/A/E, Ctrl-R,
   Up/Down history; history persisted per chat under `<state>/tui/`.
6. Status line: model, provider, running/idle with elapsed, pending
   approvals, the turn's tokens and cost.
7. Tests, feature map, live smoke, merge.

Decisions:

- `/delete` deletes the chat's workspace (`environments/{id}/delete`, what
  the web's Delete does; there is no per-chat delete route) after a typed
  confirmation.
- Prompt history lives in `<state>/tui/history/<chatID>` (the CLI kept no
  client state before; `<state>/app/` is the service's).
- The JSON export keeps each entry's JSON as the service sent it, so
  fields the client does not model (item 1's typed cards) survive.

Also decided while building:

- Ctrl+O hides tool steps and thinking altogether ("steps hidden" in the
  status); the default keeps them, as before, with Tab/`/expand` for full
  output. `/verbose` is the same toggle.
- Up/Down move between the draft's lines and recall history from its
  first/last line (Ctrl+P/N always recall); transcript scrolling is the
  wheel, PgUp/PgDn and Home/End on an empty draft.
- A lone Escape is reported after 60 ms (the decoder used to wait for the
  next key), so Esc interrupts on its own. Shift+Tab is decoded and left
  for item 3.
- `ChatCommands` on `tui.App` is the hook for commands the chat offers
  (item 5's `slash_commands`); nil today.

Verified: `go test ./internal/tui` (`-race` too), `go vet`, `gofmt`;
live on a cloned home (`~/.warden-p2`, build 66c3e37) driving the real
TUI under a pty (`scratchpad/tui_smoke.py`): `/att` → menu → Tab →
`/attach note.txt` uploaded and showed above the status; the message
carried it (Claude read the file); status line `idle 3.6s · 94k tokens
(94k in, 226 out) · $0.02`; `see @hel` listed `hello-from-tui.txt` and
Tab completed it; `/export json all FILE` (format `warden-chat`, the
attachment in the entry); `/rename` seen by `warden chat list`; Esc on a
running `sleep 90` turn → "interrupting the agent" → `interrupted 7.9s
· 46k tokens`; Up recalled the prompt, Ctrl+R found an earlier one,
Ctrl+O hid the steps, Ctrl+C twice quit; `/delete` asked, `n` cancelled,
`y` deleted the workspace and archived the chat.

Seen on the way (not this item): Claude Code emits a `result` for a
background-task notification, so the engine ends the turn (status idle,
turn record closed) while a foreground Bash of that turn is still
streaming; item 2 (background tasks) should handle that.

Left: long commands (`/delete` stops the sandbox first) block the redraw
for a few seconds; `/attach` accepts one file per command; vim mode.

Progress: started 2026-09-17; implemented and live-verified 2026-09-17
(66c3e37); merged to main c60d938 (2026-09-17) after merging item 1's
typed tool cards in (tui_test.go's append-append conflict kept both).

### Item 7: workspace `.claude/` loading, verified

Branch `feat/parity-5-slash-commands` (with item 5), 2026-09-17.

How it was probed, so it can be repeated on the next pinned CLI: a cloned
Warden home (`.local/clone-warden-home.sh p3 18830`), one Claude chat and
one message so a sandbox holds a resident CLI; `warden-sbx exec <wc-…>`
into that sandbox; the resident process's environment read from
`/proc/<pid>/environ` (proxy, `ANTHROPIC_BASE_URL` through the gateway,
the placeholder OAuth token, `HOME`) and exported for a second CLI run
with Warden's exact `AgentCommand` flags, one `user` frame on stdin and
every frame saved. Layouts: a workspace with `.claude/commands/x.md`,
`.claude/skills/y/SKILL.md`, `.claude/agents/z.md`, `.claude/rules/*.md`,
`CLAUDE.md` with an `@import`, `settings.json` and `settings.local.json`
each with a `UserPromptSubmit` hook that touches a file, and a `.mcp.json`
stdio server; the question "what secret words do you know" tells which
instruction files reached the model. Runs under `--setting-sources=`,
`project`, `user,project,local`, and `project` with
`--settings '{"disableAllHooks":true}'`; then `/x args`, `/y`,
`/no-such`, `/etc/hosts …`, `/`; `/compact` as a second message; two
messages 1 s and 12 s apart; `grep -a` over the binary for `rewind_files`
and the control subtypes. Two traps: the gateway only injects the provider
credential while the chat's resident session is alive (10 min idle), after
which the probe's calls fail with 503 `provider credential unavailable`;
and the SDK-MCP `initialize` handshake, when not answered, delays the first
API call by several seconds, which merges messages meant to be queued.

Findings are in "Decisions needed" and "To verify on the pinned CLI"
above. The launch flags were not changed here; the recommended change is
`--setting-sources=project` + `--settings '{"disableAllHooks":true}'`
(hooks off until the owner says otherwise), which also makes the
workspace's commands and skills appear in item 5's menu.

### Items 3 and 4: permission modes and plan mode

Branch `feat/parity-3-permission-modes`, worktree
`.local/warden-parity-3-permission-modes`, from main 8f0b720 (2026-09-17).

What the pinned CLI (2.1.272) does, probed inside a sandbox with a second
CLI driven over stream-json (the resident one's env and argv):

- `set_permission_mode` is a control request `{"subtype":
  "set_permission_mode","mode":…}` answered with `{"subtype":"success",
  "response":{"mode":…}}`; accepted between turns and mid-turn (even with a
  `can_use_tool` pending), followed by a `system`/`status` frame carrying
  `permissionMode`. Modes: `default`, `acceptEdits`, `plan`, `dontAsk`,
  `auto`, `bypassPermissions` — the last refused ("not launched with
  --dangerously-skip-permissions"), an unknown mode refused with the list.
- In `default` mode the CLI asks (`can_use_tool`) for Write/Edit, for
  Bash commands that write (`touch`, with `blocked_path`) or are not in
  its read-only set (`python3 -c`, `curl`), and not for `echo`, `ls`,
  `git status`. The request carries `tool_name`, `input`, `description`
  (Bash's own, or the file name), `permission_suggestions` (the CLI's own
  rule ideas: `addRules` with the *exact* command, `addDirectories`,
  `setMode acceptEdits`), sometimes `decision_reason`.
- A `{"behavior":"deny","message":…}` answer becomes the tool's error
  result (`is_error`, the message verbatim); the model reads it and reacts
  (it asked a follow-up question in the probe).
- `updatedPermissions` on an allow answer is honoured: `addRules … destination
  session` stopped later asks for that rule; `setMode` switches the CLI's
  mode (status frame follows).
- Plan mode: the CLI writes the plan file itself (`~/.claude/plans/*.md`, no
  ask), then `ExitPlanMode` arrives as `can_use_tool` with `input.plan`
  (markdown) and `input.planFilePath`, `requires_user_interaction`. A deny
  with a message makes the model revise and call it again; an allow with
  `updatedPermissions: [{type: setMode, mode: acceptEdits}]` switches the
  CLI and the tool result tells the model the plan is approved.
- `EnterPlanMode` is a tool the CLI allows itself (no `can_use_tool`); the
  only sign is the `system`/`status` frame with `permissionMode: plan`.

Design (as implemented):

1. Warden's modes are per chat, on the chat record (`Chat.Mode`: `auto`
   the default, `ask`, `plan`), owner-settable at any time through
   `chats/{id}/mode`, the web selector beside the model, `/mode` on both
   surfaces, Shift-Tab in the TUI. Claude chats only; Codex keeps its
   approval policy (`on-request` with the sandbox as the boundary) and the
   selector is hidden — mapping onto Codex's approval policy was not
   trivial enough to do blind with the account's Codex usage exhausted.
2. The adapter forwards every `can_use_tool` except AskUserQuestion to the
   engine as `item/tool/requestPermission` (the tool, its input, the item-1
   typed item, the CLI's description, the plan for ExitPlanMode); the
   engine decides under the chat record: `auto` accepts at once, a
   matching allow-always rule accepts, otherwise an approval card. The
   engine's reply `{decision, message, mode}` becomes the CLI's
   `behavior`/`message`, with `updatedPermissions: [setMode]` when the
   answer moves the mode (plan approval). Rules stay Warden's (persisted,
   survive a session restart); the CLI's session rules are not used.
3. CLI mode = `plan` for Warden's `plan`, `default` otherwise (`auto` is
   Warden allowing everything; nothing changes on the CLI side). The
   adapter sends `set_permission_mode` on the engine's `permissions/set`
   only when the CLI mode differs from the last it set or saw; the engine
   pushes the mode after `thread/start` and before every `turn/start` (so a
   new session takes the chat's mode) and live from `SetMode` when a
   session is up; a refused live push leaves the stored mode, re-applied at
   the next turn.
4. `system`/`status` frames with `permissionMode` reach the engine as
   `permissions/modeChanged`: the CLI entering plan mode by itself
   (EnterPlanMode) flips the chat to `plan` with a transcript marker;
   leaving it without Warden's approval falls back to `ask`.
5. Allow-always rules (`Chat.Allowed`): `{tool, command}` — a Bash rule is
   the command's program (its first two words for `git`, `npm`, `go`,
   `docker`, `kubectl`, `gh`, `cargo`, `pip`, `make`…; the exact command
   when it chains with `&&`, `|`, `;` or substitutes), any of the file tools
   is one rule `edit`, every other tool its name. The card says what
   "Allow always" would remember.
6. Transcript markers are `notice` entries (a new role rendered like a
   system line, dim on the TUI, quoted in an export).
7. Plan card answers: approve with `auto`, approve with `ask`, or keep
   planning with feedback (a deny whose message is the feedback).

8. A denial's message reaches the model inside the CLI's own rejection
   wording ("The user doesn't want to proceed with this tool use… To
   tell you how to proceed, the user said: …"), with a plan-mode variant:
   live, a bare message came back to the model as the tool's output and
   it took it for a prompt injection and retried the call; in the CLI's
   wording it followed the instruction.

Decision left to the owner: which modes a collaborator (non-owner) may
set. Today `chats/{id}/mode` and the approval answers are admitted like
every chat route (the edge's role check decides who reaches them);
nothing distinguishes owner from collaborator per mode. Options: (a)
everyone with chat access sets any mode; (b) collaborators may only
tighten (auto → ask → plan) and answer asks, the owner alone loosens
and answers "allow always"; (c) modes and permission answers are
owner-only, collaborators only send messages.

Progress: implemented and live-verified 2026-09-17 (b928bad); merged to
main 5f38f23 (2026-09-17) after merging items 2, 5, 6 and 7 in. Left:
the collaborator policy above; workspace-wide (cross-chat) allow-always
rules; a rules editor.

Verified (2026-09-17): `gofmt -l`, `go vet ./...`, `go test ./...`
(`agent/claude_test.go`: ask forwarded and typed, deny wording, plan
approval with `setMode`, `permissions/set` deduped and refused, status
reported; `chats/permissions_test.go` through a scripted stream-json
CLI: rules, the route, asks per mode with allow-always and a denial,
plan feedback and approval into ask, the CLI's own plan-mode status and
a live push; `tui/tui_test.go`: Shift-Tab decoding and cycling, `/mode`,
the cards and typed answers); `pnpm build`, `pnpm test` (123 tests;
`composer.test.ts` `/mode`, `permissions.test.ts`). Live on a cloned
home (`~/.warden-p5`, pinned CLI 2.1.272): an ask-mode chat — `touch`
asked (allowed once), `python3 -c` asked (allowed always: the rule
answered the next `python3` without asking, also after a Warden
restart), Write asked with its diff (denied with a message: the model
renamed the file as told), edits allowed always; a plan-mode chat — the
plan card, feedback sent it back revised, approval into auto ran the
edits and a command without asks; a mid-turn switch auto → plan during
a `sleep` — the CLI accepted `set_permission_mode` inside the turn and
the model planned instead of writing, approval into ask made the Write
ask; the web UI (selector, `/mode plan` from the composer, the command
card, the diff card, the plan card rendered as markdown, the markers);
the TUI in a pty (Shift-Tab auto → ask → plan, `/mode`, the status
line, the command and diff cards, `a`, `n <message>`, `y`).

### Item 5: slash-command pass-through

Design (see the feature map for paths):

- Adapter: `system/init` → `thread/started` now carries `commands`
  (`[{name, description?}]`, built from `slash_commands` minus
  `terminal_slash_commands`), `model`, `permissionMode` and
  `outputStyle`; `system/compact_boundary` → a system entry naming the
  token counts so a `/compact` turn is not empty (item 8 replaces it with
  a proper marker). Codex is untouched (it never sends `thread/started`
  with these fields).
- Engine/store: `Chat.Commands` kept on the chat, in `GET state` /
  `events` as `chat.commands`; cleared when the provider changes.
- Web composer: the `/` menu lists the local commands first (`stop`,
  `model`, `export`, `clear`) then the chat's commands under a "Claude"
  group; picking one puts `/name ` in the composer, sending it sends the
  text unchanged; an unknown `/x …` is still sent as text (the CLI answers
  `Unknown command: /x`).
- TUI: its `/` menu should read `chat.commands` too — follow-up under item
  6, not done here.

Verified 2026-09-17 (branch 22951f7 on a cloned home `~/.warden-p3`,
CLI 2.1.272): `GET state` carries `chat.session` (`claude-sonnet-5`,
`default`, `default`) and 39 `chat.commands` (the terminal-only
`doctor`/`color`/`reload-plugins` and `__remote-workflow` filtered);
`/context` sent through `warden chat send` renders the CLI's context table
as the reply; `/compact` completes the turn with the system line "Context
compacted: 46k → 1.1k tokens."; a workspace command the agent wrote
(`.claude/commands/hello.md`) is answered `Unknown command: /hello` under
the current `--setting-sources=` (the pass-through is intact, the CLI
does not load it), and with a throwaway build using the recommended
`--setting-sources=project --settings '{"disableAllHooks":true}'` it is
listed and `/hello world` expands to `HELLO-FROM-WORKSPACE world` (run as
a skill). In the browser, `/` opens the menu with the Chat group first and
the Claude group after, `/comp` + Enter fills in `/compact ` with the hint
as the note while the argument is typed, and Cmd-Enter sends it (48k →
1.1k). Unit tests: `TestClaudeInitCommandsAndCompaction`,
`TestSessionCommandsInStateAndCompactionNote`, `composer.test.ts` "agent
commands in the composer".

Progress: started 2026-09-17 on `feat/parity-5-slash-commands` from main
5ff4767; implemented and live-verified 2026-09-17 (22951f7); merged to
main 32138ea (2026-09-17) after merging items 1, 2 and 6 in (claude.go's
`system` case now one switch with item 2's task frames). Left: the TUI's
`/` menu does not yet list `chat.commands` (it sends "/name …" as text
all the same); descriptions for the built-ins are a static table until
the CLI sends them.