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
16. **Scrollback rendering** (TUI only). Render as Claude Code's TUI
    does: into the terminal's normal buffer, final entries written once
    and left in the scrollback, only a live tail redrawn in place; no
    alternate screen, no mouse tracking, so the terminal's own scrolling,
    search and text selection work on the whole conversation.

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
| Queue a message during a turn | ✅ | ✅ | item 10: Warden's queue (`queue.ts`, `tui/queue.go`), held by Stop; Codex steers |
| Edit a queued message | ✅ | ✅ | item 10: edit withdraws it into the composer, ↑ the last one, withdraw drops it |
| Esc to interrupt | ✅ | ✅ | `turn/interrupt` |
| Esc-Esc / edit-and-resend | ✅ | ✅ | item 10: rewind the conversation (and the code if asked) then send; Esc-Esc on the chooser, which prefills |
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
| Model choice | ✅ | ✅ | per chat; the session's resolved model shown |
| Change model mid-session | ✅ | ✅ | item 9: `set_model` on the live session; Codex relaunches |
| Effort level | ✅ | ✅ | item 9: `apply_flag_settings` `effortLevel`; `/effort` |
| Thinking on/off, budget | ✅ | ✅ | item 9: `set_max_thinking_tokens`; `/thinking` |
| Fast mode, 1M context | ✅ | ✅ | item 9: behind `providers.claude.allowFastMode` / `allowLongContext`, off by default |
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
| `/rewind` (code, conversation, both) | ✅ | ✅ | item 11: Warden's git checkpoints for code, the CLI's `rewind_conversation` for the conversation |
| Whole-session diff | ✅ | ✅ | item 11: `chats/{id}/diff`, `SessionDiff.tsx`, `/diff` |
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
| Project `CLAUDE.md`, imports, rules | ✗ | ✗ | not loaded under `--setting-sources=` (item 7); needs `project`; viewed and edited from the memory view (item 13) |
| User-level `CLAUDE.md` | ✅ | ✅ | per principal: standing instructions in the chat store, appended to the system prompt (item 13) |
| Auto-memory | ◐ | ◐ | per sandbox home; the directory is listed and its files editable (item 13); the CLI wrote none under Warden's flags |
| `/memory` editor | ✅ | ✅ | workspace panel "Memory", TUI `/memory` (item 13) |
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

## Round 2: twenty more features (2026-09-18)

Chosen from the inventory's remaining ✗/◐ rows and the "left" notes of
items 1–15, excluding what waits on the owner's decisions (workspace
settings, project MCP, plugins, hooks, collaborator policy). Seven
bundles, one worktree and one cloned home each; bundles A–D run first,
E–G after.

### A. Titles, spend, model catalog (`feat/parity-r2-a-titles-spend`)
- [x] R2.1 **Auto-titles**: a chat titled "New chat" gets a title from its first exchange (a one-shot inside the sandbox on the cheapest model, as `/btw` runs; fallback the first message's first line); rename still wins; the sidebar and TUI show it. Merged to main fe1deaa (2026-09-18); verified as the Round 2 A section says.
- [x] R2.2 **Spend**: cost per chat (sum of turns) in the chat header/menu, per workspace in the workspace panel (all its chats), and today / 7 days / all-time totals in the admin console; Codex shows tokens without cost. Merged to main fe1deaa (2026-09-18); verified as the Round 2 A section says.
- [x] R2.3 **Model catalog from the CLI**: the picker's rows, effort levels and fast/1M availability come from `list_models` (per session, cached per provider), and disallowed costlier options show a hint instead of vanishing (item 9's leftover). Merged to main fe1deaa (2026-09-18); verified as the Round 2 A section says.

### B. Permission rules (`feat/parity-r2-b-rules`)
- [x] R2.4 **Workspace-wide allow-always**: the "Allow always" answer offers this chat / this workspace; workspace rules apply to every chat of the environment. Merged to main 72aee03 (2026-09-18); verified as the Round 2 B section says.
- [x] R2.5 **Rules editor**: allow / deny / ask rules by tool pattern (Claude's `Bash(git *)`, `Edit(src/**)` syntax) per workspace, in the workspace panel and TUI `/rules`; deny rules answer without asking in every mode, ask rules force a card even in `auto`. Merged to main 72aee03 (2026-09-18); verified as the Round 2 B section says.
- [x] R2.6 **Permission history**: what was allowed, denied or auto-answered in a chat and by whom, from the chat menu and TUI `/permissions`. Merged to main 72aee03 (2026-09-18); verified as the Round 2 B section says.

### C. Live activity, search, export (`feat/parity-r2-c-activity-search`)
- [ ] R2.7 **Live activity status**: the chat status line and sidebar dot say what the agent is doing ("Running go test…", "Editing engine.go", "Explore: 3 tool calls"), `task_progress` while a subagent runs (item 2's leftover).
- [ ] R2.8 **Search and export nesting-aware**: ⌘F, ⌘K and TUI `/find` reach nested subagent entries and `!` cards; export nests children under their parent (items 2 and 12's leftovers).
- [ ] R2.9 **TUI search across chats**: `/search <text>` over titles and transcripts of every chat, with a jump.

### D. Queue and rewind polish (`feat/parity-r2-d-queue-rewind`)
- [x] R2.10 **Edit a queued message in place**: inline on the queued card (web) and back into its slot (TUI `/edit N`). Merged to main 917fb2a (2026-09-18); "### Round 2 D" below.
- [x] R2.11 **Queue semantics**: `!` and `#` leave a held queue held, explicitly (the reason in "### Round 2 D"); `warden chat send --wait` waits for its own message's turn only, `--wait-all` for the chat (item 10's leftovers). Merged to main 917fb2a.
- [x] R2.12 **Undo a conversation rewind**: the removed tail is kept and can be restored until the next turn (restore the entries; a session that cannot un-rewind starts fresh with the recap, as item 11's fallback does). Merged to main 917fb2a.

### E. Workspace fork, resource mentions, rich reads (`feat/parity-r2-e-fork-mentions`)
- [ ] R2.13 **Fork with a copy of the workspace**: "Fork…" gains "copy the workspace" — a new environment cloned from the sandbox (the runner's clone path on both drivers) plus the forked session; markers link both.
- [ ] R2.14 **Resource mentions**: the composer's `@` menu offers the chat's shared documents, repositories and previews (`@doc:`, `@repo:`, `@preview:`) and expands them to what the agent needs; TUI too.
- [ ] R2.15 **Rich Read results**: a Read of an image shows the image in the card (today "[image]"), PDFs and notebooks show a page/cell summary.

### F. TUI: vim mode, attachments, unread (`feat/parity-r2-f-tui`)
- [ ] R2.16 **Vim mode**: `/vim on|off` persisted; normal/insert, motions, operators, `u`, `:` commands.
- [ ] R2.17 **Attachments and paste**: `/attach a b c`, a local `@file` path attaches, a paste-preview chip (items 6 and 12's leftovers).
- [ ] R2.18 **Unread and jump**: unread markers per chat in `/chats` and the sidebar, an unread divider on switch, `G`/End jumps to the bottom.

### G. Asides and shortcuts (`feat/parity-r2-g-asides-keys`)
- [ ] R2.19 **`/btw` polish**: starts the session when it was released instead of refusing, cleans the aside's session copies in the guest, and an aside can be promoted into the chat as a message (item 15's leftovers).
- [ ] R2.20 **Keyboard help**: a `?` overlay on the web listing every shortcut (⌘K, ⌘F, Esc, Esc-Esc, ↑, Ctrl-R, Shift-Tab where applicable) and TUI `/keys`; both generated from one table so they cannot drift.

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
  home, so auto-memory is per sandbox; item 13 injects each person's
  instructions into the system prompt instead and lists that directory
  in the memory view. A per-principal auto-memory (`HOME` or
  `CLAUDE_CONFIG_DIR` per person) remains open.

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
  neither, so nothing is recorded today. Item 11 tried it live: with the
  variable set, `rewind_files` restored a file changed with Edit and left
  one created with Bash in place, so Warden keeps its own git checkpoints;
  `rewind_conversation` works (keyed by the `uuid` Warden now puts on
  every user frame, durable across `--resume`) and is used;
  `fork_conversation` answers `unsupported` in `-p` mode;
  `get_workspace_diff` is the working tree against HEAD only.
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

## Deployments

- 2026-09-18: main f998301 (items 1–13, 15) deployed to `~/.warden/release`
  on the owner's Mac (a wedged SBX daemon had to be terminated first).
- 2026-09-18: main e5bf597 (the above plus round 2 A/B) deployed to GKE
  Autopilot at cloud.warden.monaddle.com (images
  `warden:v0.1.0-alpha.12-316-ge5bf597`), replacing 1a8a06f.

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
- [x] 9 Mid-session model, effort, thinking — merged to main a007b96 (2026-09-18); verified as the Item 9 section says.
- [x] 10 Queueing and edit-and-resend — merged to main d11383c (2026-09-18); verified as the Item 10 section says.
- [x] 11 Checkpoints and session diff — merged to main 4d0a05e (2026-09-17); verified as the Item 11 section says.
- [x] 12 Composer polish — merged to main 981ef68 (2026-09-17); verified as the Item 12 section says.
- [x] 13 Per-user instructions and memory — merged to main a5012c0 (2026-09-18); verified as the Item 13 section says.
- [ ] 14 Project MCP, OAuth, plugins.
- [x] 16 Scrollback rendering (Claude-style TUI) — merged to main 2445764 (2026-09-18); verified as the Item 16 section says. Not deployed to `~/.warden`.
- [x] 15 Long tail — fork, `/btw`, `/cost`, notifications, output style, the TUI title: merged to main 572d873 (2026-09-18); verified as the Item 15 section says. Prompt suggestions and the `/context` breakdown are left; share links have their own plan.

### Round 2 C: live activity, nesting-aware search and export, TUI search across chats

Branch `feat/parity-r2-c-activity-search`, worktree
`.local/warden-parity-r2-c-activity-search`, from main adbf4f5
(2026-09-18). Live-tested on a cloned home (`~/.warden-p15`) against the
guest's CLI 2.1.272.

What the CLI gives for a subagent's progress (2.1.272, live): one
`system/task_progress` frame per step of a foreground subagent,
`{task_id, tool_use_id, description, subagent_type, usage: {total_tokens,
tool_uses, duration_ms}, last_tool_name}` — the counts sit under `usage`
(item 2's note had them at the top level, from 2.1.275's docs), and
`description` is the subagent's current step in the CLI's own words
("Reading hello.txt", "Running Search current directory for
zebrafish-quokka"). A `task_updated` (`patch.status`, `end_time`) precedes
the notification; ignored. Foreground Bash gets `task_started` /
`task_notification` too (no progress).

Decisions:

1. **R2.7** The status is a pure derivation from the entries, the same on
   both surfaces: `activity.ts` (`activityLabel`) and `tui/activity.go`
   (`ActivityLabel`) read the newest entry still running — a command
   ("Running go test ./...", first line, 48 chars), a file change
   ("Editing engine.go" / "Writing hello.txt" / "Editing 3 files"), a read,
   a search ("Searching for x", "Listing dir"), a web search, a fetch
   (its host), an MCP call ("Calling name"), a subagent ("Explore agent:
   3 tool calls · Reading hello.txt": type, the larger of its child steps
   and the CLI's count, the CLI's step), any other tool by its title; the
   model's thinking and a compaction as before; "" → "Agent is working"
   (was "Agent is running"). A subagent's running child names the
   outermost running Agent card. A `!` command (a sender) and a
   background command are skipped: not what the agent does now. The
   thirty shared cases live in `tui/testdata/activity.json`, run by
   `activity.test.ts` and `tui/activity_test.go`. `chatStatusLabel`
   (`stages.ts`) serves the web status line and the sidebar dot's title;
   `StatusLine` (`tui/status.go`) puts the words in place of "running".
2. `task_progress` rides on the Agent call's item as `progress`
   (`conversation.Progress`: `activity`, `toolCalls`, `lastTool`,
   `durationMS`, `tokens`), re-sent as `item/started` so `Upsert` updates
   the card in place; the card's summary and the nested transcript's
   header show the step while the subagent runs, and the count when it
   runs ahead of the child entries.
3. **R2.8** The web find bar keeps searching the rendered text (the
   highlights need real ranges) but first opens what hides a match:
   `search.ts` `entryHits` says which entries match (text, or a step's /
   aside's detail — nested and `!` cards included) and `FindBar.tsx`
   `revealHits` opens their `<details>` ancestors (group, card, subagent
   transcript) and, when the entry's visible text still has no match,
   clicks its own "+N lines" fold (never a nested entry's, never the
   prompt/input folds), once per entry per query; a palette landing waits
   for that pass. This is what browsers do for `<details>` on
   find-in-page. The palette's rows say "Tool output · in Explore agent"
   (`Hit.parent`) and "Command by You".
4. Export (`export.ts` `exportTree`, `tui/export.go` `exportTree`): a
   subagent's entries go under its card — quoted (`> `) in markdown, a
   nested subagent quoted twice, before the card's result; `children` in
   JSON (the TUI splices `children` into the service's raw record so
   unknown fields survive); a card left out (no steps) takes its subagent
   with it; a `!` card is "### Command by You — cmd" and stays in the
   messages-only export (it is the person's, not the agent's working).
5. TUI `/find` prints the rendered lines that hold the term (item 16's
   form; the terminal's own search jumps to them); when they lack it but
   the entries have it, it shows the steps (Ctrl+O) and/or expands the
   transcript (Tab) first — which reprints the transcript — and says so
   in the notice ("(output expanded)").
6. **R2.9** `GET chats/search?q=&limit=` (`chats/search.go`) searches the
   store: every chat (archived too), titles first then entries newest
   chat and newest entry first, one hit per entry, case-insensitive with
   whitespace folded (no accent folding server-side; the web palette
   keeps its own client-side search), default 40 hits, at most 200, with
   `more`; hits carry the entry's role, sender, `parentID` and a snippet
   (`before`/`match`/`after`, as `search.ts`'s). The TUI's `/search TEXT`
   lists the hits as a numbered menu (`Insert: /search N`, Enter runs
   it); `/search N` opens the chat and prints the entry as the transcript
   renders it — its card with its steps for a subagent's entry, cut to
   `findLimit` lines around the match (`entryLines`) — under a line
   saying where it is, expanding the transcript first when the entry is
   nested or the match is in a fold. This bundle was built on the
   alternate-screen TUI (a jump scrolled the entry to the top, and the
   notice moved above the status line while scrolled); item 16 landed
   in the meantime and put the transcript into the terminal's scrollback,
   which the app cannot scroll, so a jump prints instead — the same
   thing `/find` does, and what Claude Code's local commands do.

Verified: `go vet`, `gofmt -l`, `go test ./...`, `pnpm build`, `pnpm test`
(228 tests); live on `~/.warden-p15` with a Claude chat: a turn running
`sleep`, Write/Edit, Read and a foreground Explore subagent — the web
status line and sidebar title went "Sending your message" → "Waiting for
the first reply" → "Running sleep 20 && echo slept" → "Agent is working"
→ "Explore agent: starting" → "Explore agent: 1 tool call · Running Sleep
for 15 seconds" → "… 2 tool calls · Running Search current directory for
zebrafish-…" → "… 3 tool calls · Reading notes.txt" → "Agent is idle"
(the edit and read are sub-second on this workspace and were not caught
by a 250 ms poll), the Agent card "1 tool call · 4s · Running Sleep for
15 seconds"; the TUI status "⠼ 6s Running sleep 25 && echo slept", "30s
Explore agent: 1 tool call · Running sleep 12", "43s Explore agent: 2
tool calls · Reading notes.txt". ⌘F "quokka" (only in the subagents'
transcripts and the prompts) opened the three Explore cards that held it
and left the fourth closed, 16 matches, match 5 the nested grep step;
⌘F "axolotl" (only past a `!` card's fold) unfolded that card alone; ⌘K
"zebrafish" listed "Agent step · in Explore agent" rows and opening one
landed on it (14 of 16). TUI in a pty (on the alternate-screen TUI, before
item 16 landed): `/find quokka` found it 107 lines up, `/find grep exit`
(a subagent's child only) reported "(output expanded)" with the child's
line heading the view; `/export md all` wrote the Agent cards with their
steps quoted under them and two "### Command by You" cards; `/search
zebrafish` listed 9 numbered hits ("agent step in a subagent · 06:26 ·
grep -r …"), ↓ Enter put the card at the top, `/search first message
fix` then `/search 1` opened the other chat. After merging item 16 the
TUI parts were re-based on the scrollback model (decisions 5 and 6) and
re-verified by their unit tests (`TestFindReachesNestedAndFoldedEntries`,
`TestSearchAcrossChatsListsAndJumps`, `TestEntryLines`) and one more pty
run on the merged build: `/find grep exit` printed the child's line with
"(output expanded)", `/search zebrafish` then ↓ Enter printed the Agent
card with its steps under the "tool output · … (output expanded)" line.

Left: the web's ⌘K palette still searches the browser's state rather
than the new route (it has every transcript and folds accents; a
deployment with many chats would want the route); a `!` card's output in
the web export is the same fence as a tool step's (no attribution inside
the fence); `activity` shows nothing for a streaming reply beyond
"Agent is working".

Progress: started 2026-09-18; implemented and live-verified 2026-09-18
(c661a58); merge sha recorded under "## Round 2" once landed.
### Item 16: scrollback rendering (Claude-style TUI)

Branch `feat/parity-16-scrollback-tui`, worktree
`.local/warden-parity-16-scrollback-tui`, from main 31ecf8d (2026-09-18).
TUI only (`chat/internal/tui`); `warden chat send --wait`'s `Follow`
printing is untouched.

Why: the owner wants to select text in the TUI. The client entered the
alternate screen (`?1049h`) and asked for mouse reports (`?1000h`,
`?1006h`) so the wheel could scroll its own viewport; mouse reporting is
what takes the mouse away from the terminal's selection. Mouse off on the
alternate screen does not work either: terminals then send wheel ticks as
↑/↓, which collide with ↑ = prompt recall. Claude Code (Ink) avoids all
of it by printing the transcript into the normal buffer and redrawing
only a live tail; the instruction was "make it work like in Claude".

How it works (`app.go`, `render.go`):

- **Terminal modes.** Raw mode, bracketed paste (`?2004h`) and the title
  stack (`22;0t` / `23;0t`) as before; no `?1049`, no `?1000`/`?1006`.
  The cursor is hidden only while a draw writes. At exit (`finish`) the
  cursor goes below the last tail, bracketed paste is turned off and the
  title popped; the transcript and the last status/composer stay on the
  screen, as Claude Code leaves them.
- **Committed and live.** Every draw renders the whole body — the
  transcript as `RenderBlocks` (one `Block` per entry with `Final`), the
  pending approvals, the session diff — at the terminal's width. An entry
  is final when nothing about it will change: not streaming, not a
  running tool (a background card until its notification), not a queued
  message, not a compaction or an aside under way, and not a subagent's
  card while an entry under it is still one of these. The leading run of
  final entries is the committed prefix: written to the scrollback once
  (`Frame.Commit`) and never rewritten. Everything after it is the live
  tail — streaming entries, queued cards, approvals, the one-line notice,
  the `/`/`@` menu, attachments, a confirmation, the status rows and the
  composer — and a draw rewrites it in place: cursor up by the row the
  cursor was on (`screen.cursorLine`), `\r`, `ESC[J`, the new lines.
  Lines are clipped to the width so each takes exactly one row, which is
  what makes the cursor-up count right. An unchanged tail is not written.
- **Structural changes.** The body is compared with what the scrollback
  holds (`hasPrefix`): while it still begins with the committed lines the
  draw is incremental; otherwise the screen is cleared (`ESC[2J ESC[H`,
  never `3J`: the person's earlier terminal history survives) and the
  transcript is printed again from its first entry, then the tail. That
  covers a rewind (the prefix is longer than the body), the service
  reordering entries, a todo list rewritten in place, Tab / `/expand` /
  Ctrl-O / `/verbose` when they change a card that is already printed
  (when they only change the tail, no reprint), and a background card
  committed early that then settles. A chat switch (`/switch`, `/new`,
  `/fork`), a terminal resize and Ctrl-L set `redraw` and reprint
  unconditionally. Claude Code does the same clear-and-rerender on Ctrl-L,
  resize and rewind; on `/resume` it prints the other session afresh.
  `/clear` keeps its Warden meaning (the draft and its attachments), it
  is not Claude's history clear.
- **Tail budget.** The tail is kept to at most rows − 1 so the cursor
  never has to move up past the top of the screen. A taller tail has its
  top committed early (`earlyCommit`): at least the excess, rounded up to
  the next blank line (a paragraph's or an entry's end), never the last
  two lines (the ones still changing). Greedy wrapping and per-line
  inline styling make a streaming reply's earlier lines stable, so a long
  answer goes out a paragraph at a time; a committed line that does
  change later (a folded card whose window slid) costs one reprint. If
  the chrome alone does not fit (a very small terminal) the notice, then
  the extras, are cut from the top.
- **Notices.** A notice of several lines (`/help`, `/chats`, `/memory`,
  `/rewind`, `/cost`, `/find`…) is printed into the scrollback once, like
  Claude Code's local command output, and is not reprinted by a
  structural redraw (it is above, in the terminal's history). A one-line
  notice stays under the transcript for 20 s as before.
- **What is never committed.** Found live: the engine marks a queued
  message `failed` ("delivery unconfirmed") while it attempts delivery
  and `sent` once the agent acknowledges it, so a user message is final
  only when sent (or failed on an idle chat, shown red as not delivered;
  the dim `(sent)` line the TUI used to print is gone, the web shows
  none either). The todo list is one entry the adapter rewrites in place
  with every write, this turn and the next (`todoID` is per session), so
  it is a live panel at the bottom of the transcript above the queued
  messages — as Claude Code keeps its todo list above the composer,
  never in the static transcript — and goes away once the chat is idle
  with every item done (the web and an export keep it in place).
- **Resize.** `TIOCSWINSZ` itself raises SIGWINCH and a window drag
  sends one per step, each a full reprint in the first live run;
  resize events now settle for 150 ms (`resizeSettle`) and reprint only
  when the size the last draw painted for changed.
- **Scrolling and search.** PgUp/PgDn/Home/End/wheel no longer scroll
  anything in the app (the terminal has them; Home/End move within the
  draft); the scroll hint left the status bar. `/find TEXT` prints the
  matching lines as they show on the screen (up to 20, with the count)
  so the person sees where the text occurs; the terminal's own search
  jumps to it. Kept rather than dropped because round 2 bundle C builds
  on `/find`. ↑/↓ keep items 6/10/12's meanings.
- **Widths are columns.** In the alternate screen a miscounted line only
  shifted a row until the next full repaint; in the normal buffer a line
  the terminal wraps because a rune took two columns leaves a stale row
  behind at every draw. `visibleWidth`, `wrap`, `cutVisible` and `clip`
  now measure columns (`width.go`: East Asian wide and fullwidth forms
  and Emoji_Presentation runes take two, combining marks, joiners and
  variation selectors none, U+FE0F after a symbol makes it wide), and
  `sanitize` expands a tab to four spaces. Doubtful runes count as wide:
  an over-estimate wraps early, an under-estimate corrupts the screen.
- Everything else is unchanged: the menus, paste placeholders, `!`/`#`,
  approvals, Shift-Tab, Esc, Esc-Esc, Ctrl-C/D/R, the status line's
  content (now the bottom of the tail rather than the screen's last
  row), the bell and the title.

Tests (`tui_test.go`): `TestPaintCommitsOnceAndRewritesTheTail` feeds
states through `draw` and checks the bytes (no `?1049`/`?1000`/`?1006`,
a committed line written once, the tail rewritten with the right
cursor-up and `ESC[J`, an unchanged tail not written, a rewind and
Ctrl-L clearing and reprinting once without `3J`, the exit sequence);
`TestPaintCommitsEarlyWhenTheTailOutgrowsTheScreen` streams thirty
paragraphs on a 12-row terminal (tail ≤ rows − 1, no reprint, every
paragraph written once after it is committed);
`TestMultiLineNoticesArePrintedOnce`; `TestEntryFinality`;
`TestFrameSplitsCommittedFromLiveTail`; `TestTodoListIsALivePanel`;
`TestResizeBurstReprintsOnce` (the Run loop under a fake server: one
reprint for a burst, none for an unchanged size, the start and exit
sequences); `TestColumnWidths`; the key, find and status tests adapted.

Verified live (2026-09-18, build 4d693d9 on a cloned home
`~/.warden-p17`): the real `warden chat` driven under a pty from Python
(`pty.fork`, `TIOCSWINSZ` + SIGWINCH, keys with delays, the raw byte
stream recorded and replayed through a small VT emulator with a
scrollback) on a Claude chat: a turn with `ls -la`, a `Write` and a
`cat` card and a reply; `/find beta-p16` (four matching lines printed
once); `/help` (printed once); Tab expand and collapse (no reprint — no
card was folded, the notice went to the tail); Ctrl-L (one clear and
reprint); a second turn; a resize to 60×24 (one clear and reprint after
the fix above); then a 20-paragraph story streamed on the 24-row screen
and Ctrl-C twice. From the stream: no `?1049`, `?1000` or `?1006`, no
`3J`, exactly two `2J` (Ctrl-L, resize), the largest cursor-up 11 rows
on 30 rows and 22 on 24, every paragraph of the streamed story in the
emulated scrollback exactly once (committed early, never rewritten
after), every committed entry once per segment between clears, the
transcript and the last status/composer left on the screen at exit
with `\r\n ?2004l ?25h 23;0t`. The pinned CLI offered Claude no todo
tool in this environment, so the todo panel is covered by its unit
test only.

Progress: started 2026-09-18; implemented, unit-tested and live-verified
the same day; merged to main 2445764 (2026-09-18) after merging round 2
A, B and D in (main's "sending" delivery state replaced this branch's
running-chat rule for "failed": a message is live until sent). Not
deployed to `~/.warden`.

### Round 2 B: permission rules

Branch `feat/parity-r2-b-rules`, worktree `.local/warden-parity-r2-b-rules`,
from main adbf4f5 (2026-09-18). Items R2.4 (workspace-wide allow-always),
R2.5 (the rules editor) and R2.6 (permission history), on item 3's model.

Design (as implemented):

1. A rule is a kind (`allow`, `deny`, `ask`) and a pattern in Claude Code's
   own syntax (`chats/rules.go`): `Bash(git *)` (or the CLI's `git:*`) is
   `git` alone or `git ` followed by anything, `Bash(npm test)` exactly
   that command, `*` anywhere else a glob (`Bash(git * main)`);
   `Edit(src/**)` a path glob (`**` spans directories, `*` and `?` stay
   within one) matched against the call's `file_path`/`path`/
   `notebook_path` and against every tail of it since the engine does not
   know the guest's working directory, `/…` or `//…` absolute, `~/…` under
   `/home/*` or `/root`; `Edit` covers every file tool (Write, MultiEdit,
   NotebookEdit) and `Read` every read tool (Glob, Grep, LS), as the CLI's
   own rules do; `WebFetch(domain:example.com)` the host or a subdomain;
   `mcp__warden__*` a glob on the tool name; a bare name every call of
   that tool. A chained command (`&&`, `||`, `;`, `|`, `&`, a newline; not
   `>&`) is matched part by part: an allow needs every part covered (and
   never covers a substitution), a deny or ask fires on any part — so
   `Bash(git *)` does not allow `git status && rm -rf /` while
   `deny Bash(rm *)` catches it. Leading `FOO=1` assignments are dropped
   from a part. Quotes are not parsed (a `;` inside one splits too, which
   only makes an allow harder and a deny easier).
2. Rules live in two places: the chat's (`Chat.Rules`, JSON `rules`;
   item 3's `allowed` `{tool, command}` entries are converted on open —
   a program prefix to `Bash(prefix *)`, a chained command to
   `Bash(the command)`, `edit` to `Edit`) and the workspace's, on a new
   per-sandbox record (`State.Environments[sandboxID].Rules`; the
   environment view carries them as `rules`). Each rule has an id, an
   origin (`editor`, or `always` for an "Allow always" answer, with the
   chat it came from when the rule is the workspace's), who added it and
   when. The engine consults both: a deny wins over an ask over an allow
   whichever scope holds it; within a kind the chat's rules come first.
3. Per mode (`decide` in `chats/permissions.go`): a matching deny declines
   in every mode, auto included, with the model reading "a permission
   rule of this workspace denies it (deny Bash(rm *)); do not retry it,
   find another way or ask" inside the CLI's own rejection wording; a
   matching ask makes a card in every mode; a matching allow accepts in
   ask and plan mode (auto accepts anyway); `ExitPlanMode` is always the
   owner's. Rules answer the asks the CLI raises (file edits, commands
   that write or are not in its read-only set) and are not pushed into
   the CLI's settings, so a rule on what the CLI never asks about (`Read`,
   `ls`, `git status`) does not fire; the panel says so.
4. "Allow always" takes a scope: the card has two buttons (in this chat /
   in this workspace, the rule in the title), the TUI `a` / `A` (or
   `/allow`, `/allow chat`), the route body `scope`. The rule recorded is
   `RuleFor`'s, as item 3 chose it (`Bash(git commit *)`, `Edit`).
5. Editor: the workspace panel's Permissions section (Claude chats) lists
   the workspace's rules with kind, pattern and origin ("Allow always in
   “chat” by Dan", "from the editor by the owner"), an add form (kind +
   pattern, checked by `rules.ts` before the round trip with the service's
   wording), Remove; then each chat's own rules, read-only with Remove.
   TUI `/rules` (numbered: the workspace's, then each chat's), `/rules
   add allow|deny|ask PATTERN` (a workspace rule), `/rules rm N`. Routes
   `GET|POST environments/{id}/rules`, `POST …/rules/{rid}/remove`, the
   same under `chats/{id}/rules` (GET gives the same workspace view).
6. History: every `can_use_tool` decision is recorded on the chat
   (`Chat.Permissions`, last 200; left out of the streamed state, served
   by `GET chats/{id}/permissions`): tool, a one-line summary (the
   command, the path, the URL), allow/deny, how (`auto`, `rule` with the
   rule and its scope, `card` with who answered, the rule an "Allow
   always" made, a denial's message), when. "Permissions…" in the chat
   menu opens it newest first; TUI `/permissions`.

Verified (2026-09-18): `gofmt -l`, `go vet ./...`, `go test ./...`
(`chats/rules_test.go`: the matcher table — prefixes, globs, chained
commands per kind, redirections, path globs with tails, absolute and
`~/`, domains, tool globs, bad patterns; a workspace rule from chat one's
"Allow always" answering a sibling created later, with its origin,
author and both chats' history; deny and ask rules in auto with the
model's denial text and the history in order; precedence and the history
cap; the routes; `chats/permissions_test.go`: item 3's `allowed` entries
converted on load; `tui/tui_test.go`: `A`, `/allow`, `/rules`, `/rules
add`, `/rules rm N` across scopes, `/permissions`); `pnpm build`, `pnpm
test` (195 tests; `rules.test.ts`, `permissions.test.ts`). Live on a
cloned home (`~/.warden-p14`, CLI 2.1.275): chat one in ask mode asked
for `touch`, denied once with a message (the model split its chained
command as told), then allowed always for the workspace — the rule
`allow Bash(touch *)` on the environment with the chat and the owner;
chat two on the same workspace, in ask mode, ran `touch` with no card
and its history says "workspace rule allow Bash(touch *)"; `deny
Bash(rm *)` added from the API, chat two in auto: `rm` refused without a
card, the model replied "a workspace permission rule denies Bash(rm *),
and I won't retry it or route around it"; `ask Edit(*)` in auto made a
card for a Write, with "Always allow file edits in this chat / in this
workspace" beside Deny and Allow; the panel's Permissions section listed
the three rules with their origins, refused `TodoWrite(x)` with the
service's wording before sending, added and removed
`WebFetch(domain:example.com)`; "Permissions…" listed the three
decisions newest first; the TUI in a pty: `/rules`, `/rules add ask
"WebFetch(domain:example.com)"`, `/rules rm 4`, `/permissions`, and `A`
on a `mkdir` card ("allowed always for this workspace: `mkdir`
commands", the rule on the workspace). Seen: `warden start --detach`
opens the browser on every pending card (popups), which answered one
card before the API did — `--popups none` for scripted runs.

Progress: merged to main 72aee03 (2026-09-18) after merging round 2 A
in. Not deployed to `~/.warden`.

Left: rules for what the CLI never asks about would need the CLI's own
rule channel (`updatedPermissions` per session or its settings), which
this round keeps out; the collaborator policy of item 3 still applies to
who may add rules (every admitted person, today).

### Round 2 A: auto-titles, spend, the model catalog

Branch `feat/parity-r2-a-titles-spend`, worktree
`.local/warden-parity-r2-a-titles-spend`, from main adbf4f5 (2026-09-18).

What the pinned CLI (2.1.272) does, checked on the way: `list_models`
answers at once after `initialize`, before any `system/init`, with
`{models: [{value, resolvedModel, displayName, description,
supportsEffort, supportedEffortLevels, supportsAdaptiveThinking,
supportsFastMode, supportsAutoMode}]}` (the owner's account in the
sandbox: `default` → sonnet, `sonnet`, `sonnet[1m]`, `opus`, `opus[1m]`,
`haiku`; effort levels on every row but Haiku, fast mode on the Opus
rows); the one-shot flags `--max-turns 1`, `--no-session-persistence`
and `--system-prompt` exist on 2.1.272 (the binary's own strings, then
the live run) and a `--system-prompt` of one sentence makes a Haiku
title cost a fraction of a cent (46k-token conversation → the title in
about 3 s).

Design (as implemented):

1. **Titles** (`chats/title.go`). `Chat.Titled` is "" while the chat has
   the default title (`DefaultTitle`, "New chat") and waits to be named,
   "auto" once it was, "manual" once a person named it: `CreateFrom`
   sets "manual" for a title given at creation, `Edit` sets it when the
   title changes (an archive with the same title changes nothing). After
   every turn the run loop calls `autoTitle`: nothing unless the chat is
   still untitled; on a resident Claude run it asks the runner op
   `oneshot` (Haiku, `TitleSystemPrompt` as the whole system prompt,
   "User: …\n\nAssistant: …" from the first user message and the first
   reply, each cut to 2000 runes) beside the idle session — the gateway
   serves the credential only while the run is live, so it goes at once,
   in a goroutine so the session reports idle meanwhile; `CleanTitle`
   takes the first line, strips quotes and a trailing period, cuts at 60
   runes. Codex, a run that ended with the turn (`!a.resident`), or a
   failed one-shot fall back to `FallbackTitle`: the first message's
   first non-empty line, cut at a word to 60 runes with an ellipsis. The
   store update checks the chat is still untitled, so a rename that
   landed while the one-shot ran stays. One naming per chat at a time
   (`Engine.titling`); no transcript entry; the log says which it was.
   `warden chat new` and the TUI's `/new` without a title leave it to the
   service (they used to say "Terminal chat <time>"); the web's name
   field says so in its placeholder.
2. **The runner op `oneshot`** (`sandbox/aside.go`): `aside` and
   `oneshot` share `oneShotRun` (the active streaming Claude run's
   brokered environment, `r.Model` when valid, the guest script that
   feeds stdin and adds `--tools ""`, the parsed `result`); `OneShotCommand`
   resumes nothing and adds `--max-turns 1 --no-session-persistence
   --system-prompt <r.Instructions>`; `OneShotTimeout` is a minute.
3. **Spend** (`chats/spend.go`). `Chat.Spend` (turns, input, output,
   total, costUSD, priced) is filled in by `Engine.state` from the turn
   records and never stored; `GET spend` (owner-only at the edge) sums
   every chat's turns, archived included, into today (since the local
   midnight), the last seven days and all time, each with the chats
   that had a turn and a per-provider breakdown; a turn counts at its
   end, or its start while it runs. Side questions and titles are not
   turns and are left out. Web: a `spend-chip` beside the context meter
   (`spend.ts`, the service's sum or the turns summed locally on an
   older service), a Spend block in the workspace panel (this chat and
   its siblings; "$0.06 · 139k tokens · 3 turns · 2 chats"), a Spend
   section in the admin console (`SpendView.tsx`, a table by period and
   provider). TUI: `/cost` ends with "this workspace: 2 chats · 3 turns
   · 139k tokens · $0.06" when the chat shares its workspace.
4. **Catalog** (`chats/catalog.go`). After `thread/start` on a Claude
   chat the engine calls the adapter's `models/list` (→ `list_models`,
   the answer as the reply, 10 s at most) and keeps the parsed rows
   (`ModelInfo`: value, resolved, label, description, efforts,
   adaptiveThinking, fastMode; strings bounded, at most 64 rows) in
   `State.Catalog[provider]` with the time, replaced only when they
   change; a refusal keeps the last. `View` hands them out as
   `agentOptions.models` and drops them from the state. The engine
   drives the request (not the adapter on its own) so the adapter tests'
   synchronous pipes see no unexpected frame and the engine can skip
   Codex. Web `models.ts`: `modelOptions` builds the picker's rows from
   the catalog (the `default` row becomes "Provider default" with its
   resolved model as the hint; "Claude " + displayName), the static rows
   without one; a `[1m]` row the operator has not allowed is listed
   disabled as "… (not allowed)" with `LONG_CONTEXT_HINT`;
   `effortOptions` filters the Effort select to the chosen model's
   levels (the default alone, disabled, for Haiku); `fastModeFor` keeps
   the Fast checkbox visible, disabled with `FAST_MODE_HINT` until
   allowed, and says when the model lacks it. The new-chat form's picker
   gets the catalog too. TUI `complete.go`: `/model` and `/effort` gain
   argument menus (`ModelRows`, `EffortsFor`), the 1M rows with "not
   allowed here: enable providers.claude.allowLongContext"; the default
   row inserts "default", which `/model` maps to "".

Verified (2026-09-18): `gofmt -l`, `go vet ./...`, `go test ./...`
(`sandbox/aside_test.go` the one-shot launch and op; `chats/title_test.go`
the one-shot request and the cleaned title, no entry, no second naming,
a created title kept, a rename before the turn and one landing while
the one-shot runs, the Codex / failed / non-resident fallbacks,
`CleanTitle` and `FallbackTitle`; `chats/spend_test.go` the sums, the
state's spend, the report's periods and providers, the route;
`chats/catalog_test.go` cached and exposed, a refusal, a replacement,
Codex never asked, `ParseCatalog`; `agent/claude_test.go`
`TestClaudeModelsList`; `tui/tui_test.go` the menus and the workspace
line; `edge/edge_test.go` `api/spend` owner-only), `pnpm build`, `pnpm
test` (200; `models.test.ts`, `spend.test.ts` new, `composer.test.ts`
follows the disabled 1M rows). Live on a cloned home (`~/.warden-p13`,
build d4ae6b2 then 79961e1, CLI 2.1.272): `warden chat new --provider
claude` → "New chat"; one message → 10 s later `warden chat list`
showed "Fibonacci Script with Recursion Explanation", the log
"(generated)", `titled: auto`; `chats/{id}/edit` → "Fib demo (renamed
by hand)", `titled: manual`, kept across the next turn; a second
untitled chat on the same workspace → "Counting directory entries with
wc" in the header, the sidebar and `/chats`. `GET state` carried
`chat.spend` ($0.037 after two turns) and the six catalog rows; `GET
spend` today / week / all by provider (claude and codex, the cloned
chats). Web (1280 px): the picker's rows from the catalog with the
resolved model and blurb as titles, "Claude Sonnet 5 (1M context) (not
allowed)" disabled with the config hint, the Fast checkbox disabled with
its hint, the model switched to haiku live ("Model → haiku") and the
Effort select collapsed to "Effort: default" disabled with "The chosen
model takes no effort level"; the "$0.02" chip beside the context meter;
the panel's "Workspace $0.06 · 139k tokens · 3 turns · 2 chats" and
"This chat $0.02 · 46k tokens · 1 turn"; the admin console's Spend table
(Today $0.51 · 844k tokens, 11 turns, 6 chats; all time $1.83 with a
Codex column of tokens). TUI in a pty: `/cost` with the workspace line,
the `/model` menu's six rows with hints, `/chats` with the generated
title. Codex titling is unit-tested only (usage exhausted).

Progress: started 2026-09-18 on `feat/parity-r2-a-titles-spend` from
main adbf4f5; implemented and live-verified 2026-09-18 (79961e1);
merged to main fe1deaa (2026-09-18) after merging main's TUI status-bar
rows and the "sending" delivery state in (one append-append seam in
`tui_test.go`).

Left: the composer footer is crowded (the style, mode, context and
spend controls ellipsise each other at 1280 px; the chip itself never
shrinks); the aside op's cost is not in the spend; the catalog is asked
at every session start (cheap, but a `list_models` refusal is only
logged); the TUI's `/effort` menu was not seen live.

### Round 2 D: queue and rewind polish

Branch `feat/parity-r2-d-queue-rewind`, worktree
`.local/warden-parity-r2-d-queue-rewind`, from main adbf4f5 (2026-09-18).
R2.10 edit a queued message in place, R2.11 queue semantics, R2.12 undo a
conversation rewind.

Design:

1. **Edit in place** (`POST chats/{id}/queued/{entryID}/edit {text,
   attachments?}` → the entry; `EditQueued`): the queued entry keeps its
   slot and ID, its text is replaced and, when `attachments` is given,
   its attachment set (IDs of the chat's uploads; absent keeps the set);
   the sender or the owner; a message the agent has meanwhile answers
   409 "already sent". Web: the pencil (and ↑ in an empty composer, for
   the last queued message of this person's) opens an editor on the card
   itself (`QueuedEditor`: Enter saves, Shift-Enter a newline, Esc leaves
   it as it was, each attachment removable); the draft lives in
   `Conversation.tsx` (`queuedEdit`) so a card that stops being queued —
   handed to the agent, or withdrawn elsewhere — moves the draft into the
   composer with a notice instead of losing it. TUI: `/edit N` (and ↑)
   loads the message into the composer as an `editing` (the same state
   `/memory edit` uses): Enter saves it back into its slot, Esc or Ctrl+C
   leaves it, an empty save is refused (`/withdraw` drops it), and the
   "already sent" conflict ends the edit with the draft kept to send as a
   new message. Item 10's withdraw-into-composer edit is gone from both
   surfaces (Withdraw stays).
2. **The held queue is explicit** — `!` and `#` do not release it. The
   plan's wording ("`!` and `#` release a held queue") was found
   surprising: Stop is the person's decision to keep the agent from
   continuing, and a `!` command (`!git status`, `!cat file`) is the very
   thing they run after stopping to decide what to do next; restarting
   the agent as a side effect would defeat the stop. A `#` note writes
   `CLAUDE.md`, which the agent reads at launch — after a stop it is
   often the fix the person wants in place *before* the held messages go.
   Neither is addressed to the agent (item 12's decision 1), so nothing
   is out of order when they leave the queue alone; a new message
   releases it because it *is* addressed to the agent and would otherwise
   jump the queue. Claude Code's own `!` never touches its queue either.
   So `Exec` and `AppendMemory` run beside a held queue and never fail
   (they never did), and both surfaces say so: the web composer's hint
   for a `!`/`#` draft while held ("The command runs beside the held
   queue: N messages stay held until Send on a card or your next
   message", `heldHint`), the TUI's notices ("… · N queued message(s)
   still held (/queue send lets them go)", `heldNote`); the card's Send
   and the status hint were already there.
3. **`warden chat send --wait` follows its own message** (`Follow` with a
   message ID, `followMessage`): it prints the message (with "(queued: N
   message(s) ahead)" or "(queued: sends when the agent finishes)"),
   then the entries of the turn the message opens (`TurnID` once
   confirmed) and returns when that turn's record has ended and nothing
   of it streams — "idle" when the queue moved on to the next message,
   the chat's status otherwise. The wait ends with an error when the
   message is withdrawn, fails for good (the hand-over's transient
   "Delivery unconfirmed" mark, `attempt` before `confirm`, is not final
   while the run is on), or is held in a stopped chat's queue ("the
   message is held in the queue; send it from the app, with `warden chat
   send`, or withdraw it", exit 1). `--wait-all` is the old whole-chat
   wait.
4. **Undo a rewind.** A conversation or both rewind keeps what it removed
   as `Chat.RewoundTail` — the entries from the target on (the queued
   ones the rewind withdrew last), their turn records, the marker's ID,
   how the session followed, what the session had before (the pending
   rewind it replaced; the thread, `NewSession` and `Recap` a fresh
   fallback dropped), the diff base a code rewind moved and the
   checkpoint recorded of the workspace before the restore. Clients
   never get the tail: `state()` turns it into `chat.undoRewind`, the
   marker's ID, and the marker carries `entry.rewind` `{messageID, what,
   conversation, before}`. The tail is dropped when a turn starts
   (`attempt`, `resume`, `beginAgentTurn`: `dropRewoundTail`) and by the
   next rewind (only the latest is undoable); a `!` command, a `#` note
   or a side question leave it (they start no turn) and their entries
   stay after the restored ones, since the undo splices the tail in
   place of the marker. `POST chats/{id}/undo-rewind {id, code}` →
   `UndoResult {messageID, what, entries, requeued, session, code,
   restored, removed}`, chat idle: the entries and turns go back, the
   marker goes, queued messages come back held (as after a stop), and
   the session follows as far as it can — "cancelled" when the rewind
   was still pending (`c.Rewind` back to the one it replaced), "resumed"
   when the fresh fallback had dropped the thread (the CLI refused the
   rewind, so the session still knows everything: thread, flag and recap
   restored, the next message `--resume`s it), "fresh" when the live
   session rewound (the CLI cannot un-rewind: item 11's fallback, the
   restored transcript as recap, the idle session released). A `system`
   entry says what was done ("Rewind undone: N entries restored; …").
   A code rewind is one-way in the checkpoints, so the runner's `restore`
   now records the workspace as it is under `Request.Before` — the
   marker's ID — before writing the checkpoint back (`restoreScript`
   commits the snapshot it takes anyway; `WorkspaceRestore.Before`), for
   every code and both rewind; undo with `code: true` restores that
   checkpoint and puts the diff base back (offered only when the marker's
   `before` is set: web "Undo and restore the files", TUI `/undo-rewind
   code`); otherwise the notice says the workspace stays as the rewind
   left it. A code-only rewind keeps no tail (nothing to undo). Seen on
   the way: a rewind on a chat owed a fresh session (thread dropped,
   recap kept — the state an undo of a live rewind leaves) answered
   "rewound" and left the old recap describing the pre-rewind
   transcript; `Rewind` now re-renders the recap from what it leaves.
5. Web: `EntryView.tsx` (`QueuedEditor`, the marker's `rewind-actions`),
   `Conversation.tsx` (`queuedEdit`, `saveQueued`, `overtaken`, `undo`),
   `rewind.ts` (`UndoResult`, `canUndoRewind`, `undoOffersCode`,
   `undoHint`, `undoOutcome`), `queue.ts` (`heldHint`), `api.ts`
   (`editQueued`, `undoRewind`). TUI: `tui/queue.go` (`takeQueued` in
   place, `heldNote`), `tui/rewind.go` (`/undo-rewind`, `undoHint`,
   `undoNotice`), `tui/render.go` (the marker's hint), `tui/client.go`
   (`EditQueued`, `UndoRewind`, `Chat.UndoRewind`, `Entry.Rewind`).

Verified (2026-09-18): `gofmt -l`, `go vet ./...`, `go test ./...`
(`chats/queue_edit_test.go`: edit in place keeping order, ID and
attachments, the sender/owner refusals, the conflict once sent; `!`/`#`
leaving a held queue held; undo of a pending rewind (cancelled, the
resumed session gets no `conversation/rewind`), of a live one (fresh
with the recap, the notice by the actor), of a refused one (the thread
resumed without a recap); the tail dropped by a turn and by another
rewind; a both-rewind's restore carrying `Before`, its undo restoring
the marker's checkpoint and the diff base, the withdrawn message
requeued and held, "kept" without the code; the refusals; the routes;
`sandbox/checkpoint_test.go`: `restore` with `Before` writing the ref
and the record on the local git store, restoring it afterwards, a bad
ID refused; `tui/tui_test.go`: `Follow` on one message printing its own
turn only and returning while a later turn runs, held / withdrawn /
unconfirmed-then-confirmed / failed-for-good; `/edit` and ↑ in place
with Esc, save, the empty edit and the conflict; `/undo-rewind [code]`
with the marker's hint, the confirmation, the conversation-only
refusal, and the `!`/`#` held notes), `pnpm build`, `pnpm test` (196;
`rewind.test.ts`, `queue.test.ts`). Live on a cloned home
(`~/.warden-p16`, builds e83cb84 and abdd2ad, CLI 2.1.272) through the
API and `warden chat send`: two messages queued behind a `sleep 30`,
the second edited in place (same ID and slot), the queue then going
Q1, Q2-edited (the agent answered the edited text), Q3; `send --wait`
behind two queued messages printed "(queued: 2 message(s) ahead)" and
only its own turn (39 s), and `send --wait` with a `sleep 15` message
queued *behind* it returned at its own turn's end while that later turn
ran; Stop with a message queued, then `!ls -a` and `#note` — the queue
stayed held 10 s+ (their card and line after the queued entry), then
`send-queued` released it; `--wait` on a message Stop held ended with
"interrupted: the message is held in the queue" (exit 1); `--wait-all`
printed the whole queue. Undo: a live conversation rewind (`rewound`)
undone → `session: fresh`, entries back in place after the `!` card and
the note, and the new session (a new thread) answered
"CODEWORD=ALPHA, LAST=HELD-ONE" from the recap; a pending rewind
(session released by a style change) undone → `cancelled`, `rewind`
cleared, the same thread resumed and answered all three codewords and
"LAST=THREAD-UP", `conversation/rewind` never sent; a both rewind (a
file edited and one created by the agent, restored/removed) recorded
`before` = the marker's ID, and its undo with `code: true` restored
both files (`!cat r2d.txt; ls`) and 5 entries; the kept tail survived a
service restart. Browser: the pencil opening the card's editor (dashed
accent box, "Enter saves it in its place · Shift-Enter newline · Esc
cancels", Cancel/Save), the save keeping the card queued with the new
text while the earlier queued message went as its own turn; the marker
with "Undo" and its hint, the undo putting the two entries back with the
notice line; a both marker with "Undo" and "Undo and restore the files"
("the files can come back too"), the latter's notice; the held card
("Held · the agent was stopped; send or withdraw it", Send) with the
composer hint for a `!ls` draft ("The command runs beside the held
queue: 1 message stays held …"), the `!` card landing while
"interrupted · 1 message held", Send releasing it. TUI in a pty
(`scratchpad/p16-tui.py`): `/edit` on the queued message → "editing the
queued message in place", Esc → "edit of queued message cancelled"
(still queued as it was), ↑ then Ctrl+A Ctrl+K and a new text, Enter →
"saved queued message" (same ID, still queued, the agent later answered
the edited text); Esc on a running turn → "the queued messages are
held", the held marker, `!ls` → "… · 1 queued message(s) still held
(/queue send lets them go)" and "command finished · …", `#note` →
"added to CLAUDE.md · 1 queued message(s) still held", `/queue send` →
the answer; `/undo-rewind` with nothing → "nothing to undo", `/rewind N
conv` + `y`, the marker's "/undo-rewind puts the removed messages back
(until the next turn)", `/undo-rewind code` refused on a conversation
rewind, `/undo-rewind` + `y` → "rewind undone: 2 entries restored; …"
and the `!` line.

Left: the web editor edits text and drops attachments but cannot add
one (the composer's uploads are not offered to the card); the TUI's
in-place edit leaves the message's attachments as they are; a code-only
rewind is still not undoable (its `before` checkpoint is recorded, so a
later item could offer it); a fresh session after an undo knows the
restored transcript only through the recap (item 11's limit); the
pre-restore snapshot costs one more `commit-tree` per code rewind.

Progress: started 2026-09-18; implemented and live-verified 2026-09-18
(abdd2ad); merged to main 917fb2a (2026-09-18).

### Item 15: the long tail

Branch `feat/parity-15-long-tail`, worktree `.local/warden-parity-15-long-tail`,
from main bd753d4 (2026-09-17). Fork a chat, `/btw`, `/cost`, notifications,
output style, the TUI's terminal title. Share links have their own plan and
are not here; prompt suggestions and `/context` breakdowns are left.

What the pinned CLI (2.1.272) does, probed 2026-09-18 on a cloned home
(`~/.warden-p12`) inside a chat's sandbox with the resident CLI's
environment (item 7's method):

- **`--resume <session> --fork-session`** works in `-p` mode, both
  one-shot and under Warden's `--input-format stream-json` launch:
  `system/init` reports a new `session_id`, the new session knows the
  source's history (it answered with the source's codeword) and its own
  new messages, a later `--resume <new>` continues it, and the source
  session file is untouched (a second fork of the source knew only the
  source's codeword). The CLI writes the fork as a new
  `~/.claude/projects/<cwd>/<new>.jsonl` beside the source's.
  (`fork_conversation`, the control request, still answers `unsupported`.)
- **Output style.** There is no `--output-style` flag; the settings key
  `outputStyle` passed as `--settings '{"outputStyle":"Explanatory"}'`
  applies in `-p` mode: `system/init.output_style` says `Explanatory` and
  the model answered in that style (`★ Insight` blocks). An unknown name
  is echoed in `system/init` but the model runs with the default. The
  binary's built-in styles are `default`, `Explanatory` and `Learning`.
- **A one-shot side question**: `claude -p --resume <session>
  --fork-session --output-format stream-json --verbose --tools ""` with the
  question on stdin answers from the session's context in ~3 s for
  $0.03, with `system/init.tools` empty (the model cannot act, only
  answer), and leaves the resident session untouched. Without stdin
  redirected the CLI waits 3 s for it. The gateway injects the provider
  credential only for a live run, so the side question needs the chat's
  resident session up (idle is fine).

Design (as implemented):

1. **Fork** (`POST chats/{id}/fork {turnID?}` → `{id}`): a sibling chat on
   the same workspace (provider, model, mode, allow-always rules, size
   and repository copied) whose transcript is the source's up to and
   excluding the chosen user message (everything when none), turn
   records and attachments included, entry IDs kept (they are the CLI's
   message uuids), closed by a `fork` marker entry (`Entry.Fork` names the
   source chat and message; "Forked from “<chat>”[ at “<message>”]"). The
   source must be idle. A Claude chat with a session forks it: the fork
   carries the source's `ThreadID` and `Chat.ForkSession`; its first run
   sends both to the runner (`Request.ForkSession` on `prepare`, which
   drops the binding's thread instead of recording the source's, and on
   `stream`, which resumes the source with `--fork-session`); the CLI's
   `system/init` then names the new session, which `thread/started`
   records, clearing the flag. A fork cut at an earlier message gets a
   `PendingRewind` to that message, applied by item 11's
   `applyPendingRewind` right after the forked session starts (fresh
   session with the kept transcript as recap when it cannot). Codex, or
   a chat without a session, forks as a fresh session with the copied
   transcript re-sent as a recap (item 11's fallback, the preamble saying
   the chat was forked). Web: "Fork…" in the chat menu and on a user
   message's hover bar (`ForkDialog.tsx`, the message chooser like
   rewind's); `/fork`. TUI: `/fork` lists the messages, `/fork N` forks
   before message N, `/fork all` the whole chat.
2. **`/btw <question>`** (`POST chats/{id}/aside {text}`): a Claude chat
   whose resident session is up and idle (refused with a notice while a
   turn runs, when the session was released, and for Codex); the engine
   records an `aside` entry (the question as `Text`, `Sender` set,
   streaming) and asks the runner (op `aside`, the active run's broker
   config reused, a guest Python script feeding the question on stdin to
   the one-shot command above with the chat's model and output style,
   3 min timeout, output capped); the runner parses the stream-json
   (`sandbox/aside.go`: the `result`'s text, cost, usage, duration, error)
   and the entry completes with the answer as `Detail` and `Entry.Aside`
   {cost, tokens, duration, error}. Never sent to the session: the recap
   and the history skip asides. One aside at a time per chat.
3. **`/cost`**: local on both surfaces from `conversation.turns` (turns,
   tokens by kind, cost, wall time of the turns), a dismissable card on
   the web (`cost.ts`), a notice block on the TUI (`tui/cost.go`); Codex
   shows tokens without cost.
4. **Notifications.** Web (`notify.ts`, `favicon.ts`): with the tab hidden,
   a browser Notification on a turn's end, an approval or permission
   request, or a failure — from the state diff, so every chat is covered
   — deep-linking to the chat (`?chat=`); permission asked from the chat
   menu's "Desktop notifications" toggle, the choice in `localStorage`
   (`warden-notify`); a favicon badge counts what arrived while hidden
   and clears when the tab is seen. TUI: the bell on the same events
   (`/bell on|off`, kept in `<state>/tui/bell`), the terminal title
   `Warden · <chat> · running|idle|approval` (OSC 0, pushed at start and
   popped at exit with xterm's title stack).
5. **Output style** (`POST chats/{id}/style {style}` → `Chat.OutputStyle`,
   Claude chats; `default`, `Explanatory`, `Learning`): a launch setting
   (`--settings {"outputStyle":…}` through `BrokerConfig.OutputStyle`),
   so the chat's idle session is released and the next message starts
   the process with it; the selector beside the model says so, and
   `chat.session.outputStyle` shows what the running session has. TUI
   `/style [name]`.
6. Seen on the way: the SBX exec API refuses an empty argument (item
   11), so the one-shot's `--tools ""` is added by the guest script, not
   passed; a `/style` change ends the idle session, so a `/btw` right
   after it is refused until the next message (the refusal says so).

Verified (2026-09-18): `gofmt -l`, `go vet ./...`, `go test ./...`
(`sandbox/aside_test.go`: the launch flags, the one-shot command, the
guest script against a fake CLI with the timeout, the parser, the op
through the fake guest, a fork's prepare and stream; `chats/fork_test.go`
through the scripted stream-json CLI: a whole fork's transcript and
session hand-over, a cut fork's rewind on the copy, the Codex fresh
fallback with the recap, side questions on an idle session and every
refusal, the style's release and launch flag, the three routes;
`tui/tui_test.go`: `/cost` `/style` `/bell` `/fork` `/btw`, the aside
card, the title and the bell's events), `pnpm build`, `pnpm test` (173;
`cost.test.ts`, `notify.test.ts` new). Live on a cloned home
(`~/.warden-p12`, build 8ea4897, CLI 2.1.272) with a Claude chat given
two codewords: `chats/{id}/aside` answered both in 1.8 s for $0.05 with
the chat still idle on its session and the agent, asked next, unaware of
it; refused during a `sleep 25` turn; a whole fork continued on a new
session (71c20bc5) knowing a third codeword the source, resumed on
3885ea8f, never learned; a fork cut before the second codeword's message
got the pending rewind on its copied session (ac571b69) and knew only
the first; `/style Explanatory` relaunched the same session with
`--settings {"outputStyle":"Explanatory"}` (seen on the process),
`session.outputStyle` reporting it and the answer carrying `★ Insight`
blocks; an unknown style refused. Browser: the aside card, `/btw` from
the `/` menu and the composer (the question icon, the note, the card
arriving over the stream), the `/cost` card (6 turns, 325k tokens,
$0.09), Fork… from the menu and from a message's hover action, "Open the
fork" switching to it with the marker linking back, the style selector
showing "Learning (next session)" with the running style in its title,
the menu's notifications toggle reporting the pane's blocked permission,
and — with the page counted hidden — a turn's end badging the favicon
"1" and the badge clearing on visibility; the Notification itself could
not be shown there (the embedded pane denies the permission). TUI in a
pty: `/cost`, `/bell`, `/style`, `/fork` listing, `/btw` answered
($0.03) with the aside card, `/fork 2` switching to the fork, the title
`Warden · probe · idle` → `running` → `idle` around a turn with one bell,
the title pushed at start and popped at exit.

Progress: started 2026-09-18; implemented and live-verified 2026-09-18
(8ea4897); merged to main 572d873 (2026-09-18) after merging items 9,
10 and 13 in (append-append seams in the runner, the engine, the TUI,
the web composer and components, the docs; the merged build smoke-tested
on the cloned home: a turn, a side question, the launch flags).

Left: a side question needs the resident session up (the gateway's
credential is per run), so after the idle release the person sends a
message first; the aside's copy session files accumulate in the guest's
`~/.claude/projects/` (a few hundred KB each); the browser Notification
was not seen live; Codex forks and side questions are unit-tested only
(the account's Codex usage was exhausted); prompt suggestions and the
`/context` breakdown from item 15's list are not done.

### Item 10: queueing and edit-and-resend

Branch `feat/parity-10-queue`, worktree `.local/warden-parity-10-queue`,
from main bd753d4 (2026-09-18). Started 2026-09-18.

Today's behaviour, as found in the engine (`chats/engine.go`) and to be
confirmed live:

- **Claude.** A message sent while a turn runs is held by Warden, not by
  the CLI: `MessageFrom` appends the user entry with `Delivery: queued`
  and leaves the chat `running`; the turn loop's steering tick is
  Codex-only ("Claude queues a separate turn"), so the CLI never sees a
  second `user` frame during a turn and item 7's findings (the CLI's own
  queue, the merge before the first API request, `interrupt_cancel_queued_v1`)
  never come into play. After `turn/completed`, `settleTurn` marks the
  chat `queued` and `awaitMessage`'s tick → `resume` hands the first
  queued entry to a new `turn/start` on the same session; several queued
  messages go in order, one turn each, attachments delivered with each.
  The web shows "Queued" beside the sender and the hint "Queued for the
  next turn"; `canResend` disables retry/edit on a queued entry; the TUI
  prints `(queued)` under the message. Nothing edits or withdraws it.
- **Codex.** The tick sends the queued entry into the running turn with
  `turn/steer` (`expectedTurnId`); accepted, it is confirmed as part of
  that turn; refused (the turn completed first), it stays queued for the
  next.
- **Stop** fails every queued entry ("Stopped before delivery") on all
  three of its paths (off the queue, interrupt, cancel); a run that fails
  marks them "Not delivered"; a restart "Interrupted before confirmed
  delivery" (they were never handed over: `attempt`/`resume` mark the
  entry before `turn/start`, `confirm` after).
- **Rewind** refuses while the chat is `queued`; `truncate` keeps queued
  user entries after the target.
- **Edit-and-resend** (web) puts the message's text and uploads into the
  composer; sending is a new message at the end, no rewind. "Retry" sends
  the same text again as a new message.

Design:

1. **The queue is Warden's**, as it already is: the CLI's own queue
   cannot be edited, merges messages that arrive before the first API
   request, and is dropped by an interrupt. A queued entry stays a
   transcript entry (`Delivery: queued`), so both surfaces already show
   it in place and the SSE state carries it. Order is the transcript's.
2. **Withdraw** (`POST chats/{id}/withdraw {id}`) removes a queued entry
   and returns it; the sender or the owner may. **Edit** on either
   surface is a withdraw whose text and attachments land in the
   composer (Claude's ↑ pops the queued message into the input: nothing
   is sent while it is being edited); sending it again appends it to
   the queue. ↑ in an empty composer with a queued message edits the
   last queued one, before the prompt history.
3. **Stop holds the queue**: the interrupted turn ends, the chat is
   `interrupted`, the queued entries stay queued and are not sent until
   the person says so — `POST chats/{id}/send-queued` (the card's Send,
   the TUI's `/queue send`) or any new message, which goes behind them.
   A held entry says so on the card. A run that fails or a restart still
   fail them (retry is the fix there).
4. **Rewind withdraws the queue** for a conversation or both rewind
   (the marker counts them; `RewindResult.Withdrawn`); a code-only
   rewind leaves it. Since Warden never hands a message to the CLI
   during a turn, the CLI's `commands_queued` refusal is never met.
5. **Edit-and-resend** on a sent message: the composer enters an editing
   state with the message's text and uploads and a bar saying that
   sending rewinds the conversation to before it, with "also rewind the
   code" (`what: both`); send = `chats/{id}/rewind` then
   `chats/{id}/message`, by the editor (`Sender`). The pencil is enabled
   only while a rewind would be (`canRewind`). Retry is unchanged.
   Esc-Esc with an empty draft opens item 11's chooser on the last
   message, and a conversation/both rewind from the chooser (any
   rewind, not only Esc-Esc's) prefills the composer with the rewound
   message — Claude's `prefillText` — when the draft is empty. TUI:
   `/edit N` (N as `/rewind` lists) pops a queued message into the
   editor, or confirms a conversation rewind (`/edit N both` with the
   code) and prefills; Esc-Esc on an empty draft is `/edit` on the last
   message.
6. TUI: `(queued · sends when the agent finishes)` / `(queued · held)`
   markers, `/queue` lists, `/withdraw N` (N from `/queue`), `/queue
   send` releases a held queue, ↑ on an empty draft edits the last
   queued message.

Also decided while building: a queued entry sits where it was sent, so
the earlier turn's reply used to land after it; `confirm` now moves the
entry to the end of the transcript when the agent gets it, and both
surfaces render still-queued entries last (`queuedLast`), so the
transcript reads in the order things happened. The web's ↑ and the TUI's
take the last queued message *this person* may withdraw (the sender or
the owner; the TUI is always the owner).

Verified (2026-09-18): `gofmt -l`, `go vet ./...`, `go test ./...`
(`chats/queue_test.go`: order, one turn each, withdraw by sender/owner and
refusals, attachments kept through a withdraw and re-send, Stop holding
the queue with SendQueued and a new message releasing it in order, a
conversation rewind withdrawing the queue and a code rewind leaving it,
the routes; `tui/tui_test.go`: markers, `/queue`, `/withdraw`, ↑, the
held marker, `/queue send`, `/edit` confirm/rewind/prefill, Esc Esc; the
old Stop and truncate tests updated), `pnpm build`, `pnpm test` (169;
`queue.test.ts`, `stages.test.ts`). Live on a cloned home (`~/.warden-p11`,
CLI 2.1.272) through the API: two messages queued behind a `sleep 30`
turn, the second withdrawn and re-sent edited, the first withdrawn — the
edited one and a third went in order as their own turns (34.9 s, 2.3 s,
2.1 s) and the withdrawn ones never reached the agent; Stop mid-turn
held two queued messages for 10 s+ with the runner seeing no stop or
cancel (session resident), `send-queued` and a new message each released
the queue in order (HELD-A, HELD-B, NEW-AFTER-HELD); Stop on a chat
still starting held them too; a conversation rewind answered
`withdrawn: 1` and its marker says "1 queued message withdrawn"; an
edit-and-resend (rewind then message) made the agent name HELD-B as its
previous reply, the edited message forgotten. In the browser: the dashed
queued cards with "Queued · will send when the agent finishes", pencil
and X, "Agent is running · 2 queued" and the hint; ↑ in the empty
composer pulled the last queued message in; X withdrew one, the pencil
pulled the other into the composer; after Stop the card read "Held · the
agent was stopped; send or withdraw it" with Send, the status
"interrupted · 1 message held", and Send released it; the pencil on a
sent message opened the editing bar, sending rewound (marker) and the
agent's answer showed the edited message forgotten; the "also rewind the
code" checkbox gave "(code and conversation)"; Esc-Esc opened the
chooser and a conversation rewind prefilled the composer. TUI in a pty
(`scratchpad/tui_queue.py`): two `(queued · sends when the agent
finishes)` markers, `/queue`, `/withdraw 1`, ↑ editing the last queued
message and Enter re-queueing it edited, Esc → "the queued messages are
held", the held marker, `/queue send` and the answer, `/edit` asking,
`y` rewinding and prefilling, Esc Esc asking, `n` cancelling.

Left: a queued message's card cannot be edited in place (the edit goes
through the composer and to the end of the queue); a held queue is not
released by a `!` command or a `#` note; `warden chat send --wait` on a
queued message waits for the whole queue; the CLI's own queue is never
used, so nothing here exercises `commands_queued`.

Progress: started 2026-09-18; implemented and live-verified 2026-09-18
(cd4a51e); merged to main d11383c (2026-09-18) after merging items 9 and
13 in (append-append seams in `tui_test.go` and both docs only; the full
Go and web suites passed on the merged tree).

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
   keyed by principal, never in the streamed state), `GET`/`POST
   me/instructions`; web "Instructions" in the sidebar footer (a markdown
   textarea dialog; a blank save removes), TUI `/instructions` shows,
   `/instructions edit` loads them into the composer (Enter saves, Esc
   cancels), `/instructions clear`.
2. Delivery: at a launch the runner appends, after Warden's own prompt, a
   header and one block per participant — `From "<name>":` + text — for
   the chat's creator (now recorded, `Chat.Creator`) and every sender so
   far (`Request.Instructions` on the `stream` op → `RunSpec.Instructions`
   → `claudeSystemPrompt`); Codex gets the same on `developerInstructions`.
   A queued message whose sender's current text is not what the idle
   session was launched with (a late joiner; text changed or removed)
   ends the run cleanly and the chat's next run relaunches with
   everyone's current blocks (`thread/resume` keeps the conversation);
   Codex steering leaves such a message queued the same way. The launch
   passes `--system-prompt-snapshot off` (see decision 7).
3. Memory view: `GET chats/{id}/memory` lists the workspace's `CLAUDE.md`,
   `CLAUDE.local.md`, `AGENTS.md`, `.claude/CLAUDE.md`, `.claude/rules/**.md`
   (three levels) and the CLI's auto-memory directory (`memory_paths.auto`
   from `system/init`, kept as `Chat.Session.AutoMemory`, validated by the
   runner as a `memory` directory under the home's `.claude/projects`;
   derived from the workspace path otherwise) with contents (256 KiB per
   file, marked when cut); `POST chats/{id}/memory/write` `{scope, path,
   text}` replaces one file (runner op `memory-write`: staged 0644 on the
   worker host, copied into the agent home, placed by a descriptor-relative
   script that creates missing folders and follows no symlink; paths
   limited to those locations, validated on both sides). Web: a "Memory"
   section in the workspace panel (opened on demand like Access history,
   files grouped Instructions / Rules / Auto-memory, an editor dialog,
   Create CLAUDE.md / New rule…); TUI `/memory`, `/memory N|FILE`,
   `/memory edit N|FILE` (`auto:PATH` for an auto-memory file; a new
   workspace path can be edited into being).
4. Attribution: every write leaves a `notice` entry with `Sender`
   ("The owner edited CLAUDE.md", "Ada edited auto-memory MEMORY.md").
5. Tests, feature map, live check on a cloned home, merge.

Decisions:

1. Instructions live in Warden's store, keyed by principal, never in the
   sandbox: a workspace is shared by chats and people, and the sandbox
   home is the agent's; the store is where the principal already exists.
   Display name for a block: the person's name, else email, else "the
   owner" for the owner principal, else the name stored with the text.
2. Delivery is plain text in the system prompt, never a policy change,
   introduced in Warden's own voice: a bare quoted `Instructions from
   "the owner":` block was refused by the model as an injection on the
   first live probe ("that text appeared … not through any legitimate
   system or Warden channel"); with a header saying what the blocks are
   and that Warden keeps and delivers them, the same text was followed.
3. **No message prefix** (deviation from the design's "delivered once as
   a prefix"): Claude Code declines standing instructions carried inside
   a user message — live, it kept the haiku rule from the system prompt
   and refused the one in the prefix, saying genuine updates arrive as a
   system-reminder, not as chat text. A late joiner or a change relaunches
   the session instead (step 2); the cost is one CLI start (a few seconds,
   `--resume`) per change, and one prompt-cache miss.
4. The instructions ride the `stream` request as data (`AgentCommand`'s
   argument list; the drivers never go through a shell); one person's
   text is capped at 16 KiB, the assembled text at 96 KiB (cut, not
   failed: a guest argument has a hard size on Linux).
5. `GET`/`POST` rather than `PUT`: the service answers `GET` and `POST`
   only and the web client speaks those two. The write route is
   `chats/{id}/memory/write`, separate from item 12's append on
   `chats/{id}/memory`, so append and replace do not share one verb.
6. The memory listing shows what exists even though the launch reads
   none of it (item 7): the view carries `read` and a `hint` sentence
   (Claude: read only once the workspace's settings are loaded; Codex:
   reads `AGENTS.md`), which both surfaces show.
7. `--system-prompt-snapshot off` on every Claude launch. The pinned CLI
   records the system prompt at a conversation's first request and reuses
   it verbatim on every later request and resume, "even when a later
   launch passes different text, until the conversation is compacted"
   (its own option text); live, a relaunched session quoted the old
   instructions while its command line carried the new ones. Off renders
   the prompt fresh each request, which is what a relaunch needs.
8. The staged copy of a memory file goes to `<home>/.warden-memory-<id>`,
   not `/tmp`: the runtime copies it in as the host's uid with the host
   mode, and at 0600 in sticky `/tmp` the agent user could neither read
   nor remove it (the first live write failed that way).

Findings on the pinned CLI (2.1.272), for the record:

- `system/init` `memory_paths.auto` is
  `/home/agent/.claude/projects/-home-agent-workspace/memory/` (trailing
  slash; the project directory name is the workspace path with every
  character outside `[a-zA-Z0-9_-]` replaced by a dash, which the runner
  derives when the chat has no report). The CLI creates that directory at
  session start (with `sessions/`, `backups/`, the session `.jsonl`) under
  Warden's flags; across some fifteen turns in three sessions it wrote
  nothing into it, so whether `-p` mode ever writes auto-memory under
  `--setting-sources=` is not established. Files a person puts there are
  listed and editable, and readable by the agent by path.
- The agent, asked to `cat` files that appeared in its workspace between
  turns, flagged them as untrusted data and asked how they got there —
  the launch does not read them as instructions (item 7), and it does not
  treat them as such either.
- `--system-prompt-snapshot` (decision 7) and `--append-system-prompt-file`
  exist in this version.

Also fixed on the way: `conversation.css` had lost the closing brace of
`.todo-active` in item 12's merge, nesting the context meter's rules under
it, and the stylesheet brace-balance test never saw it because a `?raw`
stylesheet import is empty under vitest (the test asserted on ""); the
test now reads both files from disk (`src/node-fs.d.ts` types the one
call).

Verified 2026-09-18 on a cloned home (`~/.warden-p10`, CLI 2.1.272,
builds 7d55238 → bb2b993): unit — `go test ./...` (`sandbox/memory_test.go`
runs both guest scripts locally: the fixed files, rules three levels down,
the reported or derived auto-memory directory, symlinks never followed on
read or write, a cut file marked, folders created on write; the ops
through the fake runtime with the reported directory, path and scope
refusals, the staged copy and its placement; `claudeSystemPrompt` and the
launch flag; `chats/instructions_test.go`: the routes per principal, the
blocks, the launch's `stream` request and `developerInstructions`, the
relaunch for a late joiner / changed / removed text and none for a known
sender, the memory routes with the notice; `agent/claude_test.go` the
`autoMemory` on `thread/started`; `tui/tui_test.go` both commands and the
editing mode), `pnpm test` (161: `memory.test.ts`, the CSS test now
real). Live: instructions set through the API, a new Claude chat answered
"What is the capital of France?" as a haiku signed WARDENHAIKU; the block
read back from the CLI's `/proc/<pid>/cmdline`; `GET chats/{id}/memory`
listed the empty workspace with the auto-memory directory; writes of
`CLAUDE.md`, `.claude/rules/style.md` and `auto:MEMORY.md` through the
API, then the agent's `cat` showing all three (owned `agent agent`, 0644,
no staged file left in the home) and three notices in the transcript;
the refused `README.md`. Browser: the Instructions dialog loaded the text
with its saved stamp, a change saved and read back; the workspace panel's
Memory section listed the three files under their groups with the hint,
the editor opened `CLAUDE.md`, a saved change appeared as "The owner
edited CLAUDE.md" and the panel refreshed to 76 B. TUI in a pty: `/memory`
(listing with sizes and the directories), `/memory 1`, `/memory 3`,
`/instructions`, `/memory edit auto:MEMORY.md` → the editing banner,
Alt+Enter, Enter → "saved auto:MEMORY.md" and its notice. Changing the
owner's instructions while a session was live: the next message
relaunched the CLI (`--resume`, the new block in its command line) and,
with the snapshot off, the answer followed the new text ("two lines of
prose … GIRAFFE"); before the flag the relaunched session still quoted
the old text. Codex is unit-tested only (usage exhausted).

Left: Google mode (a second principal) is unit-tested only — the cloned
home runs in owner mode; the auto-memory question above; the memory view
needs a running sandbox (the runner refuses with "sandbox is stopped",
which the panel shows); no `#`-style append for rules or auto-memory.

Progress: started 2026-09-17; implemented and live-verified 2026-09-18;
merged to main a5012c0 (2026-09-18) after merging items 9 and 11 in
(append-append seams only); the merged build re-checked on the cloned
home (a two-line GIRAFFE answer on the resumed session, the listing).

### Item 11: checkpoints, rewind and the session diff

Branch `feat/parity-11-rewind`, worktree `.local/warden-parity-11-rewind`,
from main 0881386 (2026-09-17).

What the CLI gives (probed on the guest's 2.1.272 in Warden's launch mode,
`-p --input-format stream-json --output-format stream-json`, a second
process in a chat's sandbox with the resident CLI's environment):

- **User message ids.** A `user` frame accepts a `uuid`; any string
  works (Warden's 32-hex entry IDs were used), and the CLI keys its
  rewinds by it. `--replay-user-messages` echoes each user message back
  with its uuid (`isReplay: true`), needed only when the caller sets none.
- **`rewind_files`** `{user_message_id, dry_run?}` →
  `{canRewind, filesChanged, insertions, deletions, skippedLinks}` exists
  but is gated in `-p` mode on `CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING`
  (Warden does not set it) and is backed by the CLI's own file history:
  it restored a file changed with Edit and left a file created with Bash
  in place. Not used.
- **`rewind_conversation`** `{target_message_uuid,
  last_seen_user_message_uuid?, interrupt_if_running?}` →
  `{rewound: true, targetMessageUuid, prefillText, precedingAssistantUuid}`
  slices the resident session to before the target (the model then knew
  only what came before), and the anchor persists: a later
  `--resume <session>` continued from the rewound state; rewinding to
  before the first message works too. Without `last_seen…` any later user
  message makes the target `stale_target`; other refusals are
  `target_not_found`, `unseen_later_turn`, `turn_running`,
  `commands_queued`, `prompt_pending`. **Used** for the conversation.
- **`fork_conversation`** → `{forked: false, reason: "unsupported"}` in
  this mode. **`get_workspace_diff`** works (`{diff: {stats,
  perFileStats, hunks, skippedLarge, restricted, source}}`) but is the
  working tree against HEAD with the CLI's caps (5 s, 50 files, 1 MB per
  file), untracked files listed without hunks — not "since the chat
  started". Not used.

Decisions:

1. **Checkpoints are Warden's, in git.** Before every user turn the runner
   snapshots the workspace (tracked and untracked files, ignored ones and
   `.warden/` left out) as a tree through a temporary index — the
   working tree and the agent's index and HEAD are never touched — and
   records a root commit under `refs/warden/checkpoints/<message id>`
   (op `checkpoint`, `sandbox/checkpoint.go`). In a workspace that is a
   repository the refs live in its own `.git`; otherwise in a private git
   directory beside the workspace (`/home/agent/.warden-checkpoints.git`,
   `GIT_WORK_TREE` = the workspace), where a nested repository is
   recorded as its HEAD commit only and the snapshot is refused over
   256 MiB (`du`, dependency and build directories excluded). A snapshot
   whose tree equals the previous checkpoint's makes no new objects: the
   ref points at the previous commit. The runner keeps the records per
   sandbox (`managedSandbox.Checkpoints`, op `checkpoints`) and verifies
   the ref still names the recorded commit before restoring. A tarball
   would cost a full copy per checkpoint and give no diff; the CLI's own
   checkpoints miss what Bash does. Checkpoints are a convenience, not a
   boundary: the agent can alter refs in its own sandbox.
2. **Rewind code** (`chats/{id}/rewind` `{turnID, what}`, `turnID` the
   turn's id or the user message's) restores the checkpoint taken at that
   message: the runner snapshots the workspace as it is now, diffs it
   against the checkpoint and writes back only the paths that differ
   (`checkout-index` from a temporary index), deleting the ones the
   checkpoint lacks (op `restore`). The chat must be idle; the sandbox
   must be running.
3. **Rewind conversation** truncates the transcript to before the message
   (its turn records and pending approvals with it) and asks the agent to
   forget the same: on a live idle Claude session at once through the
   adapter's new `conversation/rewind` command (the CLI's
   `rewind_conversation`, keyed by the message id the adapter now puts on
   every user frame as its `uuid`); with no live session, recorded as
   `Chat.Rewind` and applied right after the next `--resume`, before the
   first turn. When the agent cannot rewind (Codex; a message from before
   this landed, which the CLI never saw a uuid for) the fallback is a
   fresh session: the thread is dropped (`prepare` with `newSession`
   clears the runner's binding so Claude launches without `--resume`) and
   the kept transcript is re-sent once as a preamble of the next message
   (`Chat.Recap`, last 24 KiB). Both = code, then conversation.
4. A marker entry (role `rewind`) says "Rewound to before “…” (code /
   conversation / both)"; the web renders it as a divider, the TUI as a
   line.
5. **Session diff** (`GET chats/{id}/diff`) is the workspace now against
   `Chat.DiffBase`: the chat's first checkpoint, or the one its last code
   rewind restored. The runner snapshots the workspace and answers
   `git diff-tree` between the two trees (op `diff`): a unified diff per
   file capped at 2 MiB, plus `--numstat` counts. Git-based in both
   stores, so it works for non-repository workspaces too.
6. Web: a rewind action in a user message's hover bar and Esc-Esc in the
   composer open the chooser (`RewindDialog.tsx`); "Changes" in the
   workspace panel opens `SessionDiff.tsx` over `DiffView`. TUI:
   `/rewind` lists the user messages, `/rewind N code|conv|both`,
   `/diff` shows the changed files folded, Tab expands.
7. Seen on the way: the SBX exec API refuses an empty argument (`cmd
   element N is empty`), so the scripts take `-` for "none"; the test
   harness refuses empty arguments too. `Stop` on an idle chat keeps its
   resident session (the engine's comment; the map's "released" is the
   idle timeout), so a rewind right after it still goes to the live
   session.

Verified: `go vet`, `gofmt -l`, `go test ./...`, `pnpm build`, `pnpm test`
(135 tests; `rewind.test.ts` new); live on a cloned home (`~/.warden-p7`)
with a Claude chat whose workspace is not a repository (private store):
two messages (Write, then Edit plus a Bash-made file) recorded two
checkpoints; `GET chats/{id}/diff` listed both files with git's hunks;
rewind code to before the second message restored `notes.txt` and removed
the Bash-made `extra.txt` (checked in the guest), moved the diff base and
emptied the diff; rewind conversation on the live session answered
`rewound`, cut the transcript and the agent then listed only the first
file; with the workspace stopped the rewind was `pending` and the next
message resumed the session, applied it first and the agent had forgotten
the codeword; on a chat from the previous build (messages the CLI had no
uuid for) the rewind fell back to `fresh`: the next message launched
without `--resume` and the agent called it the first message. Web: the
chat menu's Changes… (two files as folded `DiffView`s with counts), the
hover action and Esc-Esc opening the chooser on the last message, a code
and conversation rewind from it (the dialog's stopped-sandbox refusal
first, then "1 file restored, 1 file removed; … when its session
resumes"), the ↶ markers in the transcript, the panel's Changes section.
TUI in a pty: `/rewind` listing with • marks, `/diff` folded then Tab
expanded, `/rewind 2 conv` confirmed with `y` and rewound.

Progress: started 2026-09-17; implemented and live-verified 2026-09-17;
merged to main 4d0a05e (2026-09-17) after merging items 3, 4, 5, 7, 8 and
12 in (the adapter, the engine's turn start, the test fakes and the TUI
render were the conflicts; the merged build was smoke-tested live).

Left: nested repositories inside a non-repository workspace are recorded
as gitlinks (their working trees are outside the snapshot); a checkpoint
does not carry the agent's index or HEAD, so a rewind after the agent
committed leaves its commits in place and moves the working tree only;
the marker is not an undo (the removed transcript stays only in the
CLI's own session file); Codex sessions always take the fresh-session
fallback for a conversation rewind.

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

### Item 6 follow-up: the status bar wraps (2026-09-18)

The status line was one row clipped at the right edge, so on a narrow
terminal the turn's tokens and cost, the context and `/help` fell off
first. `StatusParts` now returns the bar's parts in order of importance
(title, agent, state, then the scroll hint, approvals, error, stats,
context, previews, steps hidden, `/help`) and `LayoutStatus` packs them
into rows no wider than the screen without splitting a part; the frame
budgets those rows (at most `StatusMaxRows` = 4 and a quarter of the
screen; what does not fit by then is dropped, least important last) and
the cursor follows. `Frame.Status` is the rows. Found on the way: `wrap`
measured styled text by raw runes, so escape sequences counted as columns
and a 26-column tool head (`Write hello.txt  +1 −0`) broke at 50 columns;
it now measures visible width, cuts over-long tokens by visible runes and
carries an open style across a break. Tests `TestStatusWrapsToRows`,
`TestFrameBudgetsStatusRows`, `TestWrapMeasuresVisibleWidth`; checked in a
pty at 30, 50 and 110 columns against the deployed local Warden.

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

### Item 9: mid-session model, effort and thinking

Branch `feat/parity-9-model-controls`, worktree
`.local/warden-parity-9-model-controls`, from main 05df4fa (2026-09-17).

What the pinned CLI (2.1.272) accepts, probed inside a sandbox with a
second CLI driven over stream-json with the resident one's env and argv
(the item-7 method; the probe driver answered the SDK-MCP handshake and
every `can_use_tool`, and the gateway's credential lapsed twice at the
10-minute idle mark, each time revived with one message on the chat):

- **`set_model`** `{"subtype":"set_model","model":…}` → `{"subtype":
  "success"}` (no body); the next `system/init` reports the resolved
  model (`claude-opus-5`) and the model answers as it ("I'm Opus 5 … the
  session switched models after my previous answer"). Accepted: `opus`,
  `sonnet`, `haiku`, `default` (the session default), `sonnet[1m]`,
  `opusplan`, a dated name (`claude-haiku-4-5` → `claude-haiku-4-5-
  20251001`). Refused: an unknown name, `{"subtype":"error","error":
  "Model 'bogus-model-x' not found"}`. `model` null or omitted resets to
  the default; an `@internal system_prompt` field exists. Cost: `result`'s
  `total_cost_usd` keeps running across the switch (0.054 → 0.466 → 0.535
  over sonnet → opus → haiku), with a per-model `modelUsage` breakdown, so
  the adapter's per-turn cost (growth of the total) is right as it was.
- **`list_models`** → the account's catalog: `default` (→ sonnet),
  `sonnet`, `sonnet[1m]`, `opus`, `opus[1m]`, `haiku`, each with
  `supportsEffort`, `supportedEffortLevels` (`low medium high xhigh
  max` on the Sonnet/Opus rows, none on Haiku), `supportsAdaptiveThinking`
  (Sonnet/Opus), `supportsFastMode` (the Opus rows only); `opusplan` is
  accepted by `set_model` but not listed. The binary's alias list is
  `sonnet opus haiku fable best sonnet[1m] opus[1m] fable[1m] opusplan`.
- **`set_max_thinking_tokens`** `{"max_thinking_tokens": int|null,
  "thinking_display"?: "summarized"|"omitted"|null}` → success; a
  non-integer is refused with `max_thinking_tokens must be an integer or
  null…` (a negative integer is accepted). The value maps to the CLI's
  thinking config: `0` → disabled, `n` → enabled with budget n, `null` →
  the default (adaptive on Sonnet 5 / Opus 5). Live on Haiku 4.5 (fixed-
  budget thinking, on by default): `0` → no thinking block, 0 thinking
  tokens; `2048` → a thinking block (69 tokens); `null` → thinking again.
  On Sonnet 5 the model decides: neither `0` nor `8000` produced thinking
  on the puzzles tried, so the budget is a cap there, `0` the switch off.
  Launch equivalents: `--thinking enabled|adaptive|disabled`,
  `--max-thinking-tokens N` (deprecated, `-p` only), env
  `MAX_THINKING_TOKENS` (0 = off), `CLAUDE_CODE_DISABLE_THINKING`.
- **Effort**: no `set_effort`; the control request is
  **`apply_flag_settings`** `{"settings":{"effortLevel":"low"}}` (the
  session-scoped flag layer) → success; `get_settings` then reports
  `effective.effortLevel` and `applied.effort` (`low`, `max`, back to
  `high` — the model's default — on `null`). Levels `low medium high
  xhigh max` (`max` applies although the settings schema lists only the
  first four); an unknown level is *accepted and dropped* (no error),
  so Warden validates. Launch equivalents: `--effort <level>`, env
  `CLAUDE_CODE_EFFORT_LEVEL` (which then pins effort for the session),
  the `effortLevel` setting.
- **Fast mode**: `system/init` carries `fast_mode_state` (`off`,
  `cooldown`, `on`) and `fast_mode_disabled_reason`
  (`sdk_opt_in_required` under `-p` until opted in; `not_first_party`,
  `model_not_allowed`, `disabled_by_env`…). `apply_flag_settings
  {"settings":{"fastMode":true}}` is the opt-in: the next init says
  `off` on Sonnet (no reason: the model has no fast mode) and `on` once
  the model is Opus; `false` turns it off. No `--fast` flag; the launch
  equivalent is `--settings '{"fastMode":true}'`; env
  `CLAUDE_CODE_DISABLE_FAST_MODE` forbids it.
- **1M context**: the `[1m]` aliases; env `CLAUDE_CODE_DISABLE_1M_CONTEXT`
  forbids them.
- Also there, unused: `get_session_cost`, `get_context_usage` (item 8),
  `rewind_conversation`, `fork_conversation`, `update_settings`
  (writes the project's local settings file).

Design (as implemented):

1. **Model.** `chats/{id}/agent` on a Claude chat with a live session
   sends `model/set` → `set_model` and keeps the session (`Chat.RunID`,
   the thread and the CLI's context unchanged); the store follows with a
   `notice` marker "Model → opus". A "not found" refusal is returned to
   the caller and nothing changes; any other refusal falls back to the
   old path (record, release the session, relaunch with `--model`). A
   Claude chat's model may change while a turn runs (the CLI applies it
   to the next model call); a Codex chat's still waits for idle and
   relaunches (its app-server's `turn/start` has `model` and `effort`
   fields — 24 in this build — but the account's Codex usage was
   exhausted, so that path is untouched and unverified). The CLI's
   resolved model rides on `thread/started` as before
   (`chat.session.model`); the web's picker shows it on the chosen option
   ("Claude Opus · claude-opus-5") and the TUI's status line as
   "opus (claude-opus-5)".
2. **Settings.** `Chat.Thinking` ("" default, "off", or a budget in
   tokens), `Chat.Effort` ("" default, else a level), `Chat.Fast`;
   `POST chats/{id}/settings` `{thinking?, effort?, fast?}` sets what is
   present (`Engine.SetSettings`), Claude chats only, each change a
   `notice` marker. Pushed at once to a live session (`thinking/set` →
   `set_max_thinking_tokens`, `effort/set` and `fastMode/set` →
   `apply_flag_settings`) and, with the permission mode, before every
   turn (`applySession`), so a fresh process gets them before its first
   model call; the adapter sends each only when it changes what the CLI
   has (a new process starts at the defaults). Launch flags were not
   used: the push is one code path and leaves the runner protocol alone.
3. **Policy.** `providers.claude.allowFastMode` and `allowLongContext`
   (config, default off; Helm `providers.claude.allowFastMode` /
   `allowLongContext`) reach the engine as `AllowFastMode` /
   `AllowLongContext` and clients as `GET state` → `agentOptions`
   `{fastMode, longContext}`. Fast mode on is refused unless allowed; the
   `[1m]` models are refused by `Create` and the agent route unless
   allowed (`sandbox.ValidateAgent` now admits the suffix). The picker
   offers the "Claude Sonnet 1M" / "Claude Opus 1M" rows and the Fast
   checkbox only when allowed; the CLI's `fast_mode_state` is
   `chat.session.fastMode` for the checkbox's title and the TUI's `/fast`.
4. **Surfaces.** Web: beside the model, "Thinking: default / off / 4k /
   16k / 32k" and "Effort: default / low … max" selects (Claude chats),
   the Fast checkbox when allowed; `/thinking on|off|<tokens>` (8k
   accepted) and `/effort <level>|default` in the composer, run locally
   like `/mode`. TUI: `/thinking`, `/effort`, `/fast on|off` (bare: what
   is set), the status line adds "thinking off", "effort low", "fast"
   when set.

Verified (2026-09-17): `gofmt -l`, `go vet ./...`, `go test ./...`;
`pnpm build`, `pnpm test` (138 tests; `composer.test.ts` thinking,
effort and the 1M rows); `deploy/helm/warden/test.sh` (goldens
unchanged: the switches render only when true). Unit: `agent/claude_test.go`
`TestClaudeModelThinkingEffortAndFastMode` (each request's shape, the
refusal path, dedup, the init's fast-mode state);
`chats/settings_test.go` (validation and markers, the route's policy,
the live switch keeping the run, the not-found refusal, the fallback
release and the settings pushed to the new session, a switch during a
running turn); `config/config_test.go` (the switches parse, the secret
rule still applies); `tui/tui_test.go` `TestThinkingEffortAndFastCommands`.
Live on a cloned home (`~/.warden-p9`, CLI 2.1.272, build be53f5d): a
Claude chat on sonnet, `chats/{id}/agent` → opus — `GET state` kept
`runID` and `conversation.threadID`, the sandbox's CLI PID stayed 334,
the marker "Model → opus" landed, the next reply said "I'm Opus 5
(model ID: claude-opus-5)" and `session.model` followed; turn costs
$0.02 (sonnet) then $0.46 (opus). Then haiku live: thinking off → no
thinking entry (the reply alone), a 2000 budget → "Thought for 1.0s"
entry back, default → thinking again, effort low accepted — all on the
same run. Refusals: `bogus-x` → 409 "Model 'bogus-x' not found" with
the model unchanged; fast mode and `opus[1m]` → 409 naming the config
switch. With `allowFastMode`/`allowLongContext` on (and the restart
that ended the session): fast on + opus → the new session's init
reported `fastMode: on` (the settings pushed before its first turn:
"Effort: low" carried over), `opus[1m]` set live → "claude-opus-5[1m]"
in the reply and `session.model`, fast off → `off`. Web (1280 px):
the picker reads "Claude Opus 1M · claude-opus-5[1m]", the Thinking and
Effort selects and the Fast checkbox beside it, the Thinking select →
"Thinking off" marker, `/effort hi` + Enter → the "Effort high" row and
marker, the picker → sonnet + a message → "Model → sonnet" and the
reply "I'm Sonnet 5"; at 800 px the group wraps to a second line. TUI
in a pty: status "claude · sonnet (claude-sonnet-5) · auto · thinking
off · effort high", `/thinking 8k`, `/effort low`, `/effort ultra`
(usage), `/fast` ("fast mode off (session: off)"), `/model opus` →
"opus (claude-sonnet-5)" until the reply, then "opus (claude-opus-5)",
`/fast on` during the turn → "· fast" in the status. Codex: unit-tested
only (usage exhausted).

Progress: started 2026-09-17 on `feat/parity-9-model-controls` from main
05df4fa; implemented and live-verified 2026-09-17 (be53f5d); merged to
main a007b96 (2026-09-18) after merging items 8, 11 and 12 in.

Left: Codex's model change still relaunches (its `turn/start` takes
`model`/`effort`, untested); the `thinking_display` field and the
launch flags are unused; `list_models` could replace the picker's
static Claude rows; the `[1m]` rows and the Fast checkbox are hidden
rather than explained when the operator has not allowed them.

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