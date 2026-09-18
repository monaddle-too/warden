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
| Typed tool cards (Bash, Read, Grep, Glob, Edit, Write, WebFetch…) | ◐ | ◐ | one generic card today (`claude.go` `tool_use` handling) |
| Edit/Write/MultiEdit as diffs | ✗ | ✗ | `DiffView.tsx` renders Codex `fileChange`; Claude adapter must emit it |
| Output folding ("+N lines, expand") | ◐ | ✅ | web tails 30k chars; TUI `/expand` |
| Subagent nesting, child transcript | ✗ | ✗ | `parent_tool_use_id` |
| Background task cards, task notifications | ✗ | ✗ | |
| Todo panel (TodoWrite) | ✗ | ✗ | |
| Compaction boundary marker | ◐ | ◐ | `system/compact_boundary` → a system entry with the token counts (item 5); a real marker is item 8 |
| Context-left indicator, auto-compact warning | ✗ | ✗ | `result` usage |
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
| Prompt history, Ctrl-R search | ✗ | ✗ | |
| `@path` completion | ✅ | ✗ | `paths` op |
| `@server:resource` | ✗ | ✗ | needs MCP resources |
| `/` menu with fuzzy match | ✅ (4) | ◐ typed | |
| Long paste collapsed | ✗ | ✗ | |
| Image paste, drop, picker | ✅ | ✗ | |
| File attachments into the workspace | ✅ | ✗ | |
| Queue a message during a turn | ◐ | ◐ | the CLI queues it (item 7); Warden's engine serialises turns itself; Codex steers |
| Edit a queued message | ✗ | ✗ | |
| Esc to interrupt | ✅ | ✅ | `turn/interrupt` |
| Esc-Esc / edit-and-resend | ◐ | ✗ | resident-session semantics to check |
| `!` shell command | ✗ | ✗ | policy |
| `#` append to `CLAUDE.md` | ✗ | ✗ | |
| Prompt suggestions | ✗ | ✗ | |
| Vim mode | — | ✗ | |

### Turn control, permissions, plan mode

| Feature | Web | TUI | Notes |
|---|---|---|---|
| Tool permission prompt | ✅ | ✅ | `can_use_tool` → approvals |
| Deny with a message | ✗ | ✗ | |
| Allow always (session / workspace rule) | ✗ | ✗ | `updatedPermissions` |
| Permission modes | ✗ | ✗ | `set_permission_mode`; policy per role |
| Plan mode, ExitPlanMode approval | ✗ | ✗ | |
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
| `/compact`, auto-compact | ✗ | ✗ | |
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
- **`!` shell commands.** Not probed; item 12.
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
- [ ] 1 Typed tool cards and diffs.
- [ ] 2 Subagents and background tasks.
- [ ] 3 Permission model.
- [ ] 4 Plan mode.
- [ ] 5 Slash-command pass-through.
- [ ] 6 TUI catch-up.
- [ ] 7 Workspace `.claude/` loading and policy.
- [ ] 8 Compaction and context.
- [ ] 9 Mid-session model, effort, thinking.
- [ ] 10 Queueing and rewind.
- [ ] 11 Checkpoints and session diff.
- [ ] 12 Composer polish.
- [ ] 13 Per-user instructions and memory.
- [ ] 14 Project MCP, OAuth, plugins.
- [ ] 15 Long tail.

## Items

### Item 7 — workspace `.claude/` loading: verified

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

### Item 5 — slash-command pass-through

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
