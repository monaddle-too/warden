# Thinking feedback

Branch `feat/thinking-feedback`, worktree `.local/warden-thinking-feedback`
(from `origin/main` 041c657, 2026-09-17).

## Objective

The transcript shows when the model is thinking, the way each agent's own
desktop app does, instead of nothing (Claude) or a one-line "Thinking…" tool
step (Codex, since 5fd3805 / d96889e):

- **Claude** (claude.ai / Claude Desktop): the assistant's avatar pulses
  while a reply is awaited; extended thinking is a collapsed disclosure
  headed "Thinking…" with a shimmer while it streams and "Thought for 12s"
  once done, the thinking text in muted type behind a left rule.
- **Codex** (Codex app / CLI): a shimmering "Working" line with the turn's
  elapsed time while a reply is awaited; the reasoning summary streams open
  under a shimmering "Thinking" heading (bold section headers, muted text)
  and folds to "Thought for 12s" when the model moves on.

## Steps

1. Model: a reasoning item is an entry of role `thinking` (was `activity`
   with a "Thought: …" line); its text streams by delta; `endedAt` records
   when it finished so the UI can say how long. `conversation.go`,
   `model.go`, engine dispatch of `item/reasoning/summaryTextDelta` and
   `summaryPartAdded`.
2. Claude adapter: `thinking` content blocks from the stream-json output
   become reasoning items (`item/started` → `summaryTextDelta` →
   `item/completed`), one per block. `agent/claude.go`.
3. Web: `Thinking.tsx` (the disclosure, per provider) and the pending-reply
   row at the transcript's end; `thinking` excluded from message counts,
   export and search the way `activity` is; status label "Agent is
   thinking" keyed off the new role. CSS shimmer + pulse with reduced-motion
   fallbacks.
4. TUI: a `thinking` entry renders dim like a step.
5. Tests: conversation (Go), Claude adapter (Go), transcript/export/stages
   (vitest). Live test in the local deployment with both providers.
6. Feature map row, merge to `main`, remove the worktree.

## Progress

- [x] 1–5 implemented (see commits on the branch).
- [x] Live-verified with both providers in the local deployment.
- [ ] Merged to main.

## Decisions

- One entry role for both providers; the look is decided in the browser by
  `chat.provider`, so exports and the TUI need no provider knowledge.
- Codex's raw reasoning (`item/reasoning/textDelta`) is ignored: it is off
  by default and would interleave with the summary.
- Claude's thinking is shown in full (the SDK streams it verbatim); the
  disclosure is collapsed by default as in claude.ai, so the transcript is
  not dominated by it.
- The pending row shows only while a turn runs with nothing streaming and
  no approval waiting on the owner, and not before the sandbox reaches the
  "sending" / "firstResponse" stages: earlier stages already have their own
  status line and are not the model thinking.
- `endedAt` is set for thinking entries only; other entries keep their
  shape.

## Remaining

- Merge, remove the worktree, update `AGENTS.md` in the workspace root.
