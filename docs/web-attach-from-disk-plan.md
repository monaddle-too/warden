# Attach files from this computer in the web composer

Status: started 2026-09-18 on branch `feat/web-attach-from-disk` from
origin/main 8e15ff9.

## Objective

The TUI attaches files from the machine it runs on: `/attach a b c` and a
local mention in the draft (`@./x`, `@../x`, `@~/x`), with the `@` menu
completing local paths (docs/claude-parity.md, R2.17). The web composer
has only the picker, paste and drop, and its `@` menu completes workspace
paths alone; a browser page cannot read a path the person types. On a
local Warden install the chat service runs on the person's own machine as
their user, so it can do what the TUI does: this work gives the web
composer `/attach PATH…` (`~` and globs expanded on the host, quoted
paths), local `@` mentions attached on send and rewritten to their
workspace path, and completion of local paths for both — owner-only, and
only where the service has the owner's files (`Engine.LocalMode`: a
single-owner install that is not the Kubernetes shape). Elsewhere
`/attach` opens the picker and a typed path says why it cannot be read.

## What exists

- Attachments: `chats/attachments.go` (`storeAttachment`, the 8 MiB / 8
  per message limits), `POST chats/{id}/attachments` (multipart),
  `Attachments.tsx`, `Conversation.tsx` (`addFiles`, `pending`,
  the paperclip picker), `attachments.ts` (`attachmentError`).
- Completion: `chats/paths.go` (`GET chats/{id}/paths?q=`, the worker's
  `paths` op inside the guest), `Suggest.tsx` (`usePathCompletion`,
  debounced and cached), `composer.ts` (`triggerAt`, `commandItems`,
  `COMMANDS`), the rows built in `Conversation.tsx`.
- The TUI's version: `tui/attach.go` (`AttachArgs`, `LocalMentions`,
  `IsLocalPath`, `attachMentions`), `tui/complete.go` (`localPaths`,
  `expandHome`).
- The gate: `Engine.LocalMode` (`chatsvc/main.go`: `auth.mode` owner and
  not Kubernetes), used by the host-directory grants (`grants.go`).
- `isOwner` in `chats/http.go` (the edge's `X-Warden-Role`).

## Steps

1. Service: `chats/localfiles.go` — `GET chats/{id}/local-paths?q=`
   (the TUI's `localPaths` rules: the directory the query names, entries
   whose name starts with its last segment, hidden ones only when the
   segment starts with `.`, directories first with `/`, 50 at most) and
   `POST chats/{id}/attach-local {paths, limit}` (each path `~`-expanded
   and globbed; a directory, an empty or oversized file, a pattern that
   matches nothing, and the message's count limit are errors by name; the
   stored records come back with the errors). Both refused outside
   `LocalMode` and for a non-owner. `agentOptions.localFiles` tells
   clients. Tests.
2. Web, pure parts (`composer.ts`): `attach` in `COMMANDS`,
   `isLocalPath`, `localMentions`, `attachArgs` (quoted words), the
   local-path query of an `/attach` line at the caret, `rewriteMentions`.
   Tests.
3. Web, wiring (`Conversation.tsx`, `Suggest.tsx`, `api.ts`): `/attach`
   with no path opens the picker; with paths posts them and adds the
   records to the pending list; the `@` menu completes local paths for a
   local prefix and the `/attach` argument; on send the local mentions
   are attached and rewritten, a failure keeping the draft.
4. Feature map rows, the parity doc's R2.17 note, live test on a cloned
   home, merge.

## Key decisions

1. `./` and `../` on the web resolve against the home directory: a
   browser tab has no working directory, and the prefixes stay the mark
   of a local mention on every surface (the TUI's rule).
2. The count limit is the client's to state (`limit` in the body): the
   service knows nothing of a draft's pending uploads, and globs expand
   on the host, so the check happens there.
3. Enter in the `/attach` list takes the highlighted path, `⌘Enter`
   attaches (the composer's send key; plain Enter is a newline or a
   pick, as everywhere in it). The command line clears once anything
   was attached, so a second send does not attach the files again; a
   line that attached nothing stays for a correction.
4. A mention that names no file, a directory or a glob matching nothing
   is sent as written (the TUI's rule, `missing` in the result); an
   empty or oversized file or the count limit keeps the draft.
5. Picking "Attach files…" from the `/` menu fills `/attach ` for paths
   on a local install; a bare `/attach` sent opens the picker, and so
   does the pick where typed paths are unavailable.

## Progress log

- 2026-09-18: worktree opened, plan written.
- 2026-09-18: steps 1–3 done (259a743, 2fc9ec4). Live-tested on the
  cloned home `~/.warden-p20` (build 2fc9ec4) in the browser: `/att` → Tab
  fills `/attach `; typing `~/warden-att` lists the directory, Tab takes
  it, the listing shows `sub/` first then the files; picking `my notes.md`
  inserts it quoted; `⌘Enter` on `/attach "~/…/my notes.md" ~/…/*.log
  ~/…/empty.txt` attached three chips and said `empty.txt: is empty`;
  a message with `@~/warden-attach-test/rep` completed to `report.txt`,
  was sent with the mention rewritten to `@.warden/attachments/<id>.txt`
  and `report.txt` as a fourth attachment, `@~/…/nowhere.txt` sent as
  written; Claude read all four from the workspace. A second `/attach`
  with a directory and a file attached the file, named the directory
  and cleared the line. Not verified in the browser: a bare `/attach`
  opening the picker (the paperclip's code path) and the off-install
  notes (`localFiles` false), both unit-level.
- 2026-09-18: merged to main as 929c39c (main merged in first, 7e62464: the feature-map route index took both sides). Deployed to `~/.warden/release` with main f55de51 (2026-09-18; `agentOptions.localFiles` on, service restarted). Remaining: the two browser-unverified paths above.
