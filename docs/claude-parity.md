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
| Compaction boundary marker | ✗ | ✗ | `system/compact_boundary` |
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
| Queue a message during a turn | ◐ | ◐ | verify Claude sessions; Codex steers |
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
| `/review`, `/pr-comments`, `/security-review`, `/init` | ✗ | ✗ | via pass-through |

### Slash commands, skills, agents, plugins

| Feature | Web | TUI | Notes |
|---|---|---|---|
| Built-in commands passed through | ✗ | ✗ | `system/init` lists them |
| Project commands and skills | ◐ | ◐ | verify under `--setting-sources=`; not listed |
| Custom subagents | ◐ | ◐ | verify; `/agents` ✗ |
| Plugins | ✗ | ✗ | policy |
| MCP prompts as commands | ✗ | ✗ | |
| Output styles, status line, keybindings | ✗ | ✗ | |

### Memory and instructions

| Feature | Web | TUI | Notes |
|---|---|---|---|
| Project `CLAUDE.md`, imports, rules | ✅ | ✅ | |
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

- Hooks from the workspace's `.claude/settings*.json`: allow (sandboxed) or
  keep `--setting-sources=`?
- Project MCP servers: stdio in-sandbox allowed; network servers subject to
  the gateway's destination policy; plugins fetched through the gateway?
- Permission modes offered per role (owner vs collaborator); is bypass ever
  offered?
- `!` shell commands from the composer: allowed, and attributed how?
- Per-principal instructions and memory: where do they live?

## To verify on the pinned CLI

- What `--setting-sources=` still loads from the workspace (commands,
  skills, agents, rules).
- `rewind_files` / file checkpointing availability in stream-json mode.
- Whether a user message during a running turn is queued or rejected.
- `system/init` contents (`slash_commands`, `mcp_servers`, `agents`).
- What `/compact` returns in stream-json mode.

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
