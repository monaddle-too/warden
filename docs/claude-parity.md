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
- [ ] 3 Permission model.
- [ ] 4 Plan mode.
- [ ] 5 Slash-command pass-through.
- [x] 6 TUI catch-up — merged to main c60d938 (2026-09-17); verified as the Item 6 section says.
- [ ] 7 Workspace `.claude/` loading and policy.
- [ ] 8 Compaction and context.
- [ ] 9 Mid-session model, effort, thinking.
- [ ] 10 Queueing and rewind.
- [ ] 11 Checkpoints and session diff.
- [ ] 12 Composer polish.
- [ ] 13 Per-user instructions and memory.
- [ ] 14 Project MCP, OAuth, plugins.
- [ ] 15 Long tail.

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
