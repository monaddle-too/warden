# Warden chat UI redesign

Status: implemented on branch `codex/warden-ui-redesign` (September 15, 2026),
awaiting review and deployment.

## Objective

Make the Warden chat UI readable. The previous stylesheet stacked four bars
(~290px) above the transcript, set most transcript text at 9–12px in pale
greens below WCAG AA contrast (message body 3.96:1, timestamps 2.2:1), gave
every control the same bordered-button weight, and showed approvals as raw
JSON. The visual identity was not worth preserving, so the palette changed too.

The before/after proposal that led to this work is the "Warden Chat Redesign"
design canvas (Claude artifact `8enBL97wEsKjFDdk3peYor`).

## What changed

All changes are in `chat/web`; the API and Go services are untouched.

- `src/tokens.css` (new): every colour, radius and shadow as a CSS variable,
  with a dark scheme under `prefers-color-scheme: dark`. Warm neutral surfaces,
  near-black ink text, one copper accent, semantic green/red. All text pairs
  are ≥ 4.5:1 in both schemes.
- `src/base.css`: primitives. 15px base type; three button weights (outlined,
  `.primary`, `.ghost`) plus `.chip` and `.icon`; form controls; dialogs;
  markdown output; empty and sign-in states.
- `src/chat.css`: workspace layout. 264px sidebar with status dots; a 56px
  header holding the title, a Preview toggle and a `⋯` actions menu; a 44px
  context strip of chips; preview pane; sharing dialogs; pull request review
  (now tokenised so it follows the scheme).
- `src/conversation.css`: transcript capped at 760px; user messages in a
  right-aligned bubble, agent messages plain with an avatar; grouped activity
  rows; the approval card; the composer.
- `ChatShell.tsx`: the title bar, sharing bar, environment bar and
  provider/model form collapse into header + context strip. Rename, Archive,
  Refresh, Keep running and Stop environment live in the `⋯` menu; the
  environment chip refreshes status on click; provider/model are compact pill
  selects with Save appearing only when changed.
- `Conversation.tsx` / `EntryView.tsx`: consecutive `activity` entries render
  as one collapsible `ActivityGroup`; the composer footer shows agent state
  with a status dot.
- `Approvals.tsx`: generic approvals show top-level parameters as label/value
  rows with the raw request behind a `<details>`; question options are
  toggle buttons.
- `Previews.tsx`: reports its binding count to the shell and can be hidden
  from the header Preview toggle instead of always taking 45% of the width.
- Sharing triggers (`DocumentSharing`, `RepositorySharing`,
  `PullRequestReview`) are chips; pending approvals/reviews use the accent
  `attention` style.
- `AuthRoot.tsx`: account row with avatar initial, email and a ghost Sign out.

## Validation

- `tsc --noEmit`, `vite build`, `vitest run` and `prettier --check` pass.
- Rendered against a throwaway mock of the chat API at 1440×900 (light and
  dark) and 640×900: transcript, grouped activity, approval card, actions
  menu, preview toggle, new-chat dialog and collapsed sidebar checked.
- Not exercised against the live backend: the sharing dialogs and pull
  request review were restyled through CSS only and keep their markup.

## Merges and deployment (2026-09-15)

- While this branch was being built, `codex/warden-admin-console` shipped to
  OVH twice (8b01191, then 34f62f1 with the centred Google button). Both were
  merged here (c45982c, 141ff80); `AdminConsole` was re-skinned to the tokens
  and uses the shared `.chat-header`. The sign-in button centring keeps the
  `.google-button` wrapper and its `[hidden]` rule.
- Go: `go vet` and `go test ./...` pass on the merged module; the Linux
  binaries in the release are built from it but are functionally identical to
  34f62f1 (no Go source changed on this branch).
- Release 141ff80 is staged on OVH: `/opt/warden/releases/141ff80` (source,
  `dist/ovh` binaries, `chat/web/dist`), image `warden:141ff80` built, and
  its `deploy/chat/.env` copied from 34f62f1 with `WARDEN_IMAGE` updated.
  Only the chat container needs recreating (`up -d --no-deps chat`); runner
  and policy stay on `warden:8b01191`, and the edge binary is unchanged.
- Switched 2026-09-15 ~19:15 UTC: `/opt/warden/current` → 141ff80, chat
  container recreated on `warden:141ff80` with no sandboxes running; runner
  and policy still `warden:8b01191`, edge untouched. Post-deploy checks: root
  200, signed-out `/api/state` 401, served assets `index-B1H5F9Ar.css` /
  `index-RcZgoqhD.js` match the local build. Rollback is
  `ln -sfn /opt/warden/releases/34f62f1 /opt/warden/current` and the same
  `up -d --no-deps chat`; release 34f62f1 and its image are kept.

## Remaining work

- Check the sharing dialogs, PR review and admin console against real data
  with a signed-in owner.
- `codex/warden-admin-console` must merge this branch before its next release
  or the redesign is reverted.
- The `⋯` menu uses `<details>`; if keyboard navigation between items is
  wanted, replace it with a roving-tabindex menu.
- Dark scheme follows the OS only; add a manual toggle if requested.
