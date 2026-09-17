# Chat rich rendering and composer attachments

Branch `feat/chat-rich-rendering`, worktree `.local/warden-chat-rendering`
(from `public` at 77832e6, 2026-09-17). Work is done by an agent loop: one
agent per step below, in order, each leaving a green build and a commit.

## Objective

Close the gaps between Warden's chat and the chat UIs owners already use:
render what agents emit (images, Mermaid, highlighted code, diffs, math),
let the owner send images and files to the agent, and add the transcript
conveniences (copy, export, search, jump-to-bottom, turn timing).

## Ground rules for every step

- Web code: `chat/web/src` (React 19, `react-markdown` 10 + `remark-gfm`).
  Match the existing style: small components, terse comments explaining
  *why*, CSS in `base.css` / `conversation.css` / `chat.css` using the tokens
  in `tokens.css` (no inline styles, no new CSS frameworks).
- Verify with `pnpm run build` (runs `tsc --noEmit`) and `pnpm test` in
  `chat/web`; Go with `GOPROXY=off go build ./... && go test ./internal/chats/`
  in `chat`. Add vitest tests for pure logic (`src/*.test.ts`).
- Security posture is not negotiable: the agent is untrusted. Never load a
  remote URL the agent wrote into an `<img>`, `<iframe>`, `<video>` or
  `<link>`; never `dangerouslySetInnerHTML` agent text except through a
  renderer that sanitises (Mermaid `securityLevel: "strict"`, KaTeX
  `trust: false`). Images only come through
  `/api/chats/{id}/images/{id}` (imageguard-normalised PNG) or the
  `chats/{id}/file` route re-normalised the same way.
- Keep bundle growth in check: Mermaid, the highlighter and KaTeX are
  lazy `import()`s behind the fence that needs them, never in the main
  chunk. Record the `vite build` size before/after in the commit message.
- Update the feature map row for Chats (`docs/feature-map.md`) when a step
  adds a file, route or tool.
- Commit each step on its own (`git add -A && git commit`), message in the
  repo's style (short imperative subject, body says why), ending with
  `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.

## Steps

| # | Step | Status | Notes |
|---|---|---|---|
| 0 | Worktree, plan, add `mermaid`, `rehype-highlight`, `remark-math`, `rehype-katex`, `katex` | done | build 454.6 kB before any use |
| 1 | Code component: syntax highlighting, language label, copy button, wrap toggle, collapse over ~40 lines | done | one `code`/`pre` override in `RichText.tsx`; highlighter lazy-loaded; theme via tokens for light and dark |
| 2 | Mermaid fences rendered client-side | done | `securityLevel: "strict"`, `startOnLoad: false`, lazy import; render only once the fence is closed (not while `isStreaming`); parse error → keep the code block with a small error line. Strict alone was not enough (see decisions): HTML labels off and config locked via `secure`, image nodes refused before render, SVG re-filtered after |
| 3 | Inline images in markdown | done | `![alt](relative/path)` → new `GET chats/{id}/image-file?path=` (worker `image-file` op + the same imageguard subprocess as `attach_image`, shared as `Engine.workspaceImage`; nothing stored, PNG served with the images route's headers); `InlineImage.tsx` shows a thumbnail that opens `Lightbox.tsx` (also used by `ImageAttachment`); `images.ts` accepts only plain relative paths (no scheme, host, absolute or `..`), so `http(s)`, `data:` and the rest stay as alt text; fetches are shared per (chat, entry, path) and capped at 3 in flight |
| 4 | Diff rendering | done | `diff.ts` finds unified diffs in any text (git `diff` lines, `---`/`+++` pairs that lead to a hunk, bare `@@` hunks) and keeps the prose around them; `DiffView.tsx` shows a header per file (path, new/deleted/renamed/binary, +/− counts), a fold button per hunk (long hunks start folded), +/- rows on the `--add-*`/`--del-*` tokens, numbers and marks as CSS content so a selection copies only the code. Activity `detail` switches to it when a real hunk is found (Codex `fileChange`, a `git diff` an agent ran); ```` ```diff ````/```` ```patch ```` fences always, falling back to prefix colouring for hand-written diffs without hunk headers. Text nodes only, no fetch surface |
| 5 | Math | done | `remark-math` + `rehype-katex` with KaTeX and its CSS in one lazy chunk (`katex.ts`), loaded only when `math.ts` `hasMath` finds display math, a ```` ```math ```` fence or Pandoc-style inline math, so "$5 and $10" in a message without math is left alone; `KATEX_OPTIONS`: `trust: false` (no `\href`/`\url`/`\includegraphics`/`\html*`, they render as errors), `throwOnError: false`, `strict: "ignore"`, `maxSize` 10 em, `maxExpand` 1000, error colour on the danger token; KaTeX output reaches React as hast, never `innerHTML`; wide display math scrolls in its line |
| 6 | Streaming-safe rendering | done | `streaming.ts`: while `entry.isStreaming`, `displayText` holds back the tail lines whose reading is not settled (a fence opener until its info string ends, a closing fence in progress, a pipe line until its newline, a would-be table header until its delimiter row is complete), so a table never shows as a paragraph first and a row appears whole; an unterminated fence needs no closing (CommonMark runs it to the end) but `touchesEnd` tells the block that reaches the end of the text from a settled one, so Mermaid waits only for its own fence to close (not for the whole message) and `holdOpenMath` (a remark plugin, added only while streaming) shows a formula that reaches the end as its source instead of a KaTeX error; an open fence never folds and stays expanded once it closes. Also fixed two remount bugs that made every block flicker on every chunk: the `components` map in `RichText.tsx` was rebuilt per render (a new component type to React, so every `CodeBlock` remounted and lost its state and diagram) and `Conversation.tsx` passed a fresh `onFile` to every memoised `EntryView` |
| 7 | Composer attachments: paste / drag-drop / picker for images and files | done | `POST chats/{id}/attachments` (multipart, ≤ 8 MiB) keeps a copy beside the chat state (`attachments/<chat>/<id>` + `<id>.json`); PNG/JPEG (sniffed) go through imageguard and are stored as the normalised PNG; the message names the IDs (`attachments` on `chats/{id}/message`, text may be empty) and `Entry.Attachments` records them; at turn start `Engine.input` writes each into the sandbox at `.warden/attachments/<id>.<ext>` through the new worker op `attachment-write` (host-staged, `Runtime.Copy` into the guest's `/tmp`, a privileged move that refuses a symlinked `.warden`), appends an "Attached files" note with the paths to the text, and adds a `localImage` item per image — with the PNG as base64 for Claude, whose adapter turns it into an `image` content block (12 MiB per turn); `GET chats/{id}/attachments/{aid}` serves the copy (PNG inline, files as a download), `…/remove` forgets an unsent upload, unsent uploads expire after a day; `Attachments.tsx` shows chips with remove buttons while uploading and thumbnails / download chips under the sent message; paste, drop on the composer and a paperclip picker all feed the same `addFiles` |
| 8 | Edit-and-resend / retry last message | done | every user entry has Edit and Retry buttons, shown on hover or focus (always when delivery failed, always on a touch screen); Retry posts the same text and attachment IDs under a new ID (`resendAttempt`), Edit puts the text into the composer with the caret at the end and the entry's uploads as `reused` chips (name, size and image preview from the service's copy; removing one does not remove the upload), asking before it replaces a draft or unsent uploads; both are disabled while a send would be refused (`canResend`: reconnecting, archived, queued/stopping, the entry itself still queued); `claimAttachments` now lets a chat name an attachment an earlier message carried, so a retry keeps its files |
| 9 | Copy message as markdown, export chat (markdown + JSON) | done | every user and agent message has a Copy button in its hover bar (`MessageActions` in `EntryView.tsx`, the clipboard state shared with the code block through `useCopy.ts`) that copies `entry.text`, the markdown source, and waits for a streaming message to finish; the chat menu's "Export…" opens `ExportDialog.tsx` (native `<dialog>`: Markdown or JSON, optional agent activity) and `export.ts` builds the file from `chat.conversation` in the browser — markdown keeps message text verbatim under `## Author — time` headings, lists attachments, fences tool output with a fence longer than any it contains, quotes system lines; JSON is the entries as the service sent them under a `format: "warden-chat"` header; the file name is an ASCII slug of the title plus the time (`saveFile`, which `downloadFile` now shares); nothing is asked of the service, so an archived chat exports too |
| 10 | Search: within a chat and across chat titles/entries | done | ⌘K / Ctrl+K (and a sidebar "Search" button) opens `SearchPalette.tsx`, a native `<dialog>` over the state the browser already has: chat titles and every entry's text (a tool step's output too) across live and archived chats, title hits first, then entries newest first, each with a snippet; with no query it lists the chats last worked on, so it also switches chats. An entry row opens its chat and lands on the entry (`data-entry` on transcript elements; a collapsed activity group is opened), the first row finds the query in the open chat. `FindBar.tsx` (⌘F / Ctrl+F while the transcript or composer has focus, or the header's find button) counts matches in the *rendered* text, paints them with the CSS Custom Highlight API (`::highlight(warden-find)`, nothing added to React's DOM), scrolls to the current one, steps with Enter / Shift+Enter, follows the transcript as it streams through a `MutationObserver`. `search.ts` holds the pure matching (`fold`, `findMatches`, `locate`, `snippet`, `searchChats`), unit-tested: literal substring, case- and accent-insensitive, any whitespace equal, offsets preserved |
| 11 | Jump-to-bottom button, unread divider | done | `transcript.ts` holds the pure parts (`unreadStart`, `newSince`, `readSeen`, `groupEntries` moved out of `Conversation.tsx`). Following is now state as well as the ref: once the reader scrolls away from the end, a pill button floats over the scroll box ("Jump to latest", or "N new messages" counting messages, not tool steps, that arrived after the last entry they had in view) and smooth-scrolls back (instant under `prefers-reduced-motion`; scroll events during the jump do not count as leaving again; a wheel, a touch, a scrolling key, a press on the scrollbar or `scrollend` does). What the reader has seen is the last entry that was on screen while they followed the transcript with the tab visible, stored per chat in localStorage as `{id, at}` (`warden-seen:<origin>:<chat>`); the divider goes before the entry after it, or before the first entry newer than its time if it is gone, and never above every entry or on a first visit. The entry the divider goes before is fixed once when the chat opens (`unreadEntry`, then `unreadIndex` against the live entries), so it stays put while reading and never appears above what arrives during the visit; an activity group never spans it. The label is CSS content, so a selection and the find bar skip it. A chat opens at its end unless the unread stretch is taller than the view, then at the divider with the jump button showing how much is below |
| 12 | Safe-link display | done | `links.ts` `agentLink` decides how an http(s) link an agent wrote is shown: the URL parsed by the browser (`URL`), so an internationalised or lookalike host becomes its ASCII `xn--` punycode form and credentials in the URL are dropped; the normalised URL is the hover title, an external-link glyph follows the label (`AgentLink` in `RichText.tsx`, on for assistant entries through the new `agent` prop), and the real host is shown after the link when the label would mislead: the label names another host (`[https://apple.com](https://evil.example)`, a lookalike spelling, `node.js` on a link elsewhere), the host is punycode, or the URL carried userinfo (`https://apple.com@evil.example/`). An agent link with any other scheme (`mailto:`, `tel:`) is text; a workspace path still downloads through `chats/{id}/file`. The owner's own links render exactly as before |
| 13 | Turn timing and token/cost per assistant turn | done | the service keeps a record per turn (`Conversation.Turns`: `startedAt` when the agent accepts the message, `endedAt` when the turn completes, fails or is stopped, `usage` as the provider reports it); Codex reports `thread/tokenUsage/updated` with a running total per process, so a turn's usage is the growth of that total since the turn began (`activeRun.usage`, a repeated report counts nothing); the Claude adapter turns the `result`'s per-turn `usage` and cumulative `total_cost_usd` into the same notification, cost included; `TurnStats.tsx` shows the line under the turn's last agent entry (in the message's action row, or after a tool-step group): the time from the owner's message (`createdAt`) to the end, counting up once a second while the turn runs, the tokens with the in/out split (caches and reasoning in the hover title) and the cost where the provider estimates one; the markdown export notes it under the message and the JSON export carries the records |
| 14 | Slash commands and `@path` mentions in the composer | done | `composer.ts` holds the pure parts: `triggerAt` reads a leading `/` (the first line is the command and its argument) or an `@` at the start of a word (the text to the caret is the path prefix), `commandItems` lists the commands by prefix (stop, model, export, clear draft) and, once `model` has an argument, the models by substring, `exactCommand` recognises a message that is exactly a command so `/stop` sent with ⌘Enter runs instead of reaching the agent. `Suggest.tsx` is the list floating over the composer (textarea stays the combobox: ↑↓ move over enabled rows, Enter/Tab pick, Escape puts the trigger away, rows pick on mousedown without taking the focus) and `usePathCompletion` asks the new `GET chats/{id}/paths?q=` (worker op `paths`: `sandbox/paths.go`), debounced, stale answers dropped, answers cached per chat; a directory pick keeps the list open after its slash, a file pick ends the mention with a space, a stopped sandbox shows the worker's reason in place of rows. Stop is enabled only while the agent runs, model only while it does not; `/export` opens the shell's export dialog; the mention stays in the text as written |
| 15 | Final review pass | pending | full build + tests, `docs/feature-map.md`, this plan's status column, screenshots of each renderer in light and dark |

## Key decisions

- Everything the agent writes is treated as attacker-controlled; the render
  path is the trust boundary, so rendering features are allowed only when
  they cannot fetch from the network on the agent's behalf.
- Heavy renderers are lazy chunks so the transcript's first paint does not
  pay for them.
- Mermaid's `securityLevel: "strict"` is a floor, not the whole answer. In
  a browser test it still emitted `<img>` from an HTML label and `<a>` from
  `click`, and a `%%{init}%%` `themeCSS`/`fontFamily` or an image node
  (`A@{ img: … }`) made the browser fetch *during* render, before any output
  filter runs. So `Mermaid.tsx` keeps labels as SVG text (`htmlLabels:
  false`), lists the theme/CSS/label config keys as `secure` so a directive
  cannot flip them, parses first and refuses diagrams whose database has
  image nodes, and finally strips fetching/navigating elements and
  attributes from the SVG (`mermaid.ts` `DROP_TAGS`, `unsafeAttribute`)
  before it is injected. Review then found a fourth pre-render vector:
  a `$$…$$` label in a sequence or class diagram takes Mermaid's KaTeX
  path, which ignores `htmlLabels` and writes HTML into a `foreignObject`
  in the live document after only DOMPurify's default profile, which keeps
  `<img src>`; in a browser `A->>B: $$x$$ <img src="…">` fetched during
  render. So a fence containing `$$` is refused before Mermaid loads
  (`hasMathLabels`), and as a backstop Mermaid's label sanitiser is given
  `dompurifyConfig` (`LABEL_PURIFY`: the fetching tags and referencing
  attributes forbidden; the key is `secure`), verified to stop the fetch
  on its own. The CSS filter also rejects `image-set()`, `image()`,
  `src()` and `cross-fade()`, which fetch like `url()`. A CSP on the
  served page would be a further backstop and is out of this plan's scope.
- Inline images are not attachments. `attach_image` stores an immutable
  copy in the policy service because a Doc or PR may later refer to it; an
  `![alt](path)` in the transcript is just a view of a workspace file, so
  `image-file` reads and normalises on request and stores nothing. The
  client only asks for plain relative paths (`workspaceImagePath`), the
  worker refuses symlinks and anything outside the workspace, and the
  normaliser re-encodes in a memory-capped subprocess, so an agent cannot
  make the owner's browser fetch anything it did not write into the
  sandbox. The fetch is keyed per entry, not per path, so a later message
  showing the same path after an overwrite reads the file again.
- Diffs are detected, not declared: the activity detail is one string
  (Codex's `fileChange` gives `path\ndiff`, a command's output is whatever
  the tool printed), so `parseDiff` looks for hunks in any text and the
  view keeps what is around them as prose. Hand-written diffs get their
  counts wrong, so the counts only settle the ambiguous lines (an empty
  line a tool stripped, a `---` that is a deleted `--`) and the prefix
  decides the rest; a `---`/`+++` pair without a hunk stays prose because
  `---` is also a markdown rule.
- Math is gated, not just lazy. remark-math reads any `$…$` pair as math,
  which turns "costs $5 and $10" into italic nonsense, so `hasMath` applies
  Pandoc's stricter rule (opening `$` before a non-space, closing `$` after
  one and not before a digit, on one line) and a message that fails it never
  gets the plugin. A message with both real math and prices still loses the
  prices; that is remark-math's behaviour and was judged rarer than a price
  in prose. KaTeX itself needs no output filter: with `trust: false` every
  command that could carry a URL, class or style renders as an error, colour
  arguments are validated to hex or a name, and rehype-katex hands the
  result to react-markdown as hast, so the transcript still never injects
  HTML. Verified in a browser: `\href`, `\url`, `\includegraphics`,
  `\htmlStyle{background:url(…)}` and `\textcolor{url(…)}` produced no
  `<a>`, no `<img>` and no request, `\rule{10000em}` was capped, a macro
  bomb hit `maxExpand` and rendered as its source.
- Streaming is settled by position, not by holding the whole message.
  CommonMark already closes an unterminated fence or `$$` block at the end
  of the text, so nothing is appended (a closer appended at column 0 would
  open a new empty fence after a fence inside a list or quote). Instead a
  block whose `position.end.offset` reaches the end of the rendered text is
  "open": Mermaid renders a closed fence while the rest of the message is
  still streaming, and an open `$$`/`$…$` stays as its source (the
  `holdOpenMath` remark plugin runs after remark-math, so the rest of the
  message keeps its math). Only the lines that would flicker through a wrong
  reading are held back, so text still appears as it arrives: a pipe line
  waits for its newline because micromark reads `| a | b |` + `|---|-` as a
  table the moment the delimiter row has enough cells and as a paragraph
  before, and a fence opener waits for its newline because every character
  of its info string is a different language. The holds are regex
  heuristics over the tail; a wrong guess only delays a line, never changes
  how it parses. Two pre-existing remount bugs surfaced in the browser
  probe and were fixed here because they defeat every other streaming
  measure: the `components` map handed to react-markdown must be the same
  object across renders (a new inline `pre` function is a new component
  type, so React remounted every block on every chunk), and the memoised
  `EntryView` needs a stable `onFile`.
- Attachments live in the sandbox (the agent reads them like any file)
  rather than in a host-side store, so nothing new needs sharing policy.
  The chat service does keep its own copy beside `chats.json`, for two
  reasons found while building step 7: the worker only writes into a
  *running* sandbox, and a new chat's sandbox starts with its first turn,
  so writing on upload would refuse the very first message's files;
  and the transcript should still show what was sent after the agent
  moves or deletes the file. So the upload is stored, the message names
  it, and `Engine.input` writes it into the sandbox just before
  `turn/start` (again on a retry; the write replaces). The plan's original
  idea of turning an uploaded image into an `image` entry via the policy
  service's `image_add` was dropped: it needs the sharing socket, which a
  local install may not have, and would split one message into two
  bubbles. Images are still imageguard-normalised on upload, so the agent,
  the transcript and Claude's inline image block all see the same PNG; a
  file is served back only as a download (`Content-Disposition:
  attachment`, `sandbox` CSP), never rendered. The file's name is the
  sender's, reduced to one printable path component; the path in the
  workspace is the service's own (`<id>.<ext>`), so the sender cannot
  choose where the write lands, and the worker checks that shape again.

- Copy and export are string work on the state the browser already has.
  A message's markdown is `entry.text` as the service stores it, so "copy as
  markdown" is a clipboard write of that string and the export a join of
  them; no route was added and the service is not asked for anything, which
  also means an archived chat, whose sandbox may be gone, exports like a
  live one. The markdown export leaves message text untouched (it is the
  markdown the reader wants) and describes the rest: a tool step's output
  goes into a fence one backtick longer than any run it contains, so an
  agent's ``` cannot end the fence early, and a system line becomes a
  quote. Nothing in the file is rendered by Warden, so an agent's text
  cannot do more in the file than it could in the transcript; the file
  name is built from the owner's title only, reduced to ASCII letters,
  digits, dots and dashes. Copy is disabled while a message streams so the
  clipboard never holds half of it.
- Retry and edit are client-side re-sends, not server-side operations.
  The service already treats a message as (ID, text, attachments) and
  delivers whatever the composer posts, so "retry" is the same message
  under a new ID and "edit" is the composer prefilled; nothing is
  rewritten in the transcript, the earlier entry stays as it was sent,
  and a failed delivery is retried by the same path as a deliberate
  resend. The one service change is that an attachment ID an earlier
  message of the chat carried may be named again: the stored copy lives as
  long as the chat does (pruning only forgets unsent uploads), and
  `Engine.input` writes it into the sandbox again before the turn, so a
  retried message with a screenshot still has the screenshot. The
  transcript's buttons follow the send button's rules (`canResend`) so a
  refused send shows as a disabled button rather than an error, and an
  entry that is still queued cannot be retried into a duplicate.

- Search is client-side and offset-preserving. Every chat's entries are
  already in the browser (`State.chats`), so the palette searches them
  without a route, archived chats included, and nothing is asked of the
  service. Matching is deliberately simple, one rule everywhere: the query
  is a literal substring of the folded text, where folding lower-cases,
  strips accents (NFD, combining marks removed) and turns any whitespace
  into a space, so "cafe" finds "Café" and a two-word query spans a soft
  line break. `fold` never changes a string's length (a character whose
  folding would, like "İ" or a bold 𝐀, is left as it is), so a match found
  in the folded text is the same range in the original; that is what lets
  the find bar map matches back onto DOM text nodes and the palette cut a
  snippet with the match marked. The find bar searches what the reader
  sees rather than `entry.text`: the rendered text of each transcript
  block, minus buttons, closed `<details>` and KaTeX's hidden MathML copy,
  concatenated so a match may span `**bold**` boundaries. Highlights use
  the CSS Custom Highlight API, which paints ranges without inserting
  `<mark>` elements into a tree React owns (a `<mark>` would remount
  blocks, lose code-block state and re-render diagrams); a browser without
  it still gets the count and the scrolling. The ⌘F binding is limited to
  the transcript and composer so the browser's own find keeps working on
  the other panels.
- A link's label is not evidence of its destination. An agent link is
  shown with what the browser will actually connect to: the WHATWG `URL`
  parser is the same one the browser uses to navigate, so its `hostname` is
  the ASCII host after IDNA (a Cyrillic `аpple.com` is `xn--pple-43d.com`),
  and clearing `username`/`password` removes the `https://apple.com@evil…`
  trick from both the title and the `href`. The visible host hint is kept
  for the cases where the label itself makes a claim (it names a host that
  is not the real one, as written, so a lookalike never compares equal to
  its punycode), the host is punycode (rare enough to always point out), or
  the URL had userinfo; a prose label with a plain host gets only the glyph
  and the title, so an ordinary link does not grow a second host. The
  markdown's own `title` attribute is never used: it would be the agent's
  text on hover in place of the real URL. The owner's links are shown as
  written, as the plan asked, since they wrote them.
- "Seen" is a position, not a time, and it is the reader's, not the
  service's. The service does not know which browser has shown what, and
  a timestamp alone misplaces the divider when two entries share a second
  or the clock differs, so the browser remembers the last entry that was
  on screen while it followed the transcript with the tab visible (its
  ID, plus its time as a fallback for a replaced transcript) in
  localStorage per chat, like drafts. The `follow` ref, not the `away`
  state, decides whether the mark advances: a chat that opens at the
  divider stops following in the mount layout effect, and the passive
  effect of that same commit still sees the state it closed over, so a
  state guard would mark the whole unread stretch seen on arrival. The
  divider is computed once when
  the chat opens and then stays, so it does not chase the reader down the
  page as the mark advances; the jump button's count is relative to where
  the reader left the end, not to the divider, so it is right in both the
  "scrolled up" and the "opened at the divider" cases. The transcript
  still scrolls to its end by default; it starts at the divider only
  when the unread stretch would not fit in the view, so a short reply
  looks as it always has.

- Path completion is a listing made inside the guest, bounded and
  symlink-proof, never a search the host runs over the workspace. The
  worker's `paths` op runs `pathsScript` in the sandbox with the same
  descriptor-relative, `O_NOFOLLOW` walk as `image-file`, so a symlink the
  agent planted cannot lead the listing outside the workspace; it matches
  the typed segment as a case-insensitive prefix within its directory
  (shell style, directories first and marked with a slash), and a query
  with no directory part also searches the tree for the prefix, capped at
  50 answers, 4000 entries and six levels with `.git`, `node_modules` and
  the like skipped, so `@Conv` finds a component two folders down without
  a workspace full of dependencies costing more than one bounded walk.
  Names with control characters are left out and the client shows the
  rest as text only; nothing is fetched for them. A missing directory is
  an empty answer, a stopped sandbox is the error the composer shows. The
  mention itself is plain text (`@src/x.ts`): the agent reads it as a path
  as it would in any message, so nothing is expanded or rewritten on send
  and a path with a space is inserted as is (the next completion then
  starts at the space). Slash commands are the composer's own: only a pick
  from the list or a message that is exactly `/name` runs one, and the
  command line leaves the composer while the rest of the draft stays; the
  list disables what the buttons would refuse (stop while idle, model
  while running).
- A turn's cost is measured where the provider measures it, and the
  service does no pricing of its own. Codex reports token usage after
  each model call as a running total for the process (a resident session
  spans several turns, and the same total can be reported again with a
  rate-limit update), so the turn's usage is the growth of that total
  since the turn began, kept on the run rather than in the store because
  a new process starts from zero. Claude Code's `result` gives the turn's
  own token usage but a cumulative cost estimate, so the adapter sums the
  one and differences the other into the Codex shape (`input` counts the
  cached and cache-written tokens too, as Codex's does). Codex gives no
  cost and a price table in Warden would go stale, so a Codex turn shows
  tokens only. Duration runs from the owner's `createdAt` to the
  service's own clock at the turn's end, so the sandbox boot and the queue
  count as the owner experienced them; a stopped or failed run ends its
  open turn, a restart leaves it open (its line then shows tokens only).
  The line lives in the transcript, not the header, and keeps its object
  while nothing shown changed (`sameFooter`), so a memoised entry is not
  re-rendered by every streamed chunk of a later one.

## Progress log

- 2026-09-17: step 0 done.
- 2026-09-17: step 1 done — "Render fenced code with highlighting, copy, wrap and collapse" (`CodeBlock.tsx`, `code.ts`; highlighter is a 167 kB lazy chunk, main chunk 454.6 → 458.8 kB).
- 2026-09-17: step 2 done — "Render closed mermaid fences as diagrams" (`Mermaid.tsx`, `mermaid.ts`; Mermaid is lazy chunks, main chunk 458.8 → 463.6 kB; verified in a browser that no agent-controlled URL is fetched).
- 2026-09-17: step 2 fix after review — "Refuse mermaid math labels and lock its label sanitiser": the KaTeX label path fetched before the output filter (confirmed in a browser: 4 beacon requests without the fix, 0 with it); `$$` fences refused, `dompurifyConfig` set, CSS image functions filtered, blank fences skip the chunk, the SVG walk (`scrub`) is pure and unit-tested on a fake tree. Main chunk 463.6 → 463.8 kB.
- 2026-09-17: step 3 done — "Show workspace images inline in the transcript" (`InlineImage.tsx`, `Lightbox.tsx`, `images.ts`, `chats/{id}/image-file`; main chunk 463.8 → 466.4 kB; verified in a browser that remote, `data:` and `..` sources stay as alt text and no request leaves for them).
- 2026-09-17: step 4 done — "Render unified diffs in activity detail and diff fences" (`DiffView.tsx`, `diff.ts`; no lazy chunk needed, main chunk 466.4 → 471.1 kB; verified in a browser in light and dark that an `<img>` in a diff line stays text and nothing is fetched).
- 2026-09-17: step 5 done — "Render math with remark-math and KaTeX" (`math.ts`, `katex.ts`; KaTeX and its stylesheet are lazy chunks of 259 + 11 kB JS and 29 kB CSS, main chunk 471.1 → 471.6 kB; verified in a browser in light and dark that trusted-only commands render as errors and nothing is fetched).
- 2026-09-17: step 6 done — "Keep streamed messages from flickering while they render" (`streaming.ts`, `streaming.test.ts`; no new chunk, main chunk 471.6 → 473.3 kB; verified in a browser with a chunked probe that a table never renders as a paragraph, rows and fence openers appear whole, a diagram renders as soon as its fence closes and stays, open math never shows a KaTeX error, and a long open fence does not fold).
- 2026-09-17: step 7 done — "Send files and images with a message" (`chats/attachments.go`, `sandbox/attachment.go`, `Attachments.tsx`, `attachments.ts`; no new chunk, main chunk 473.3 → 479.7 kB; verified in a browser with a stubbed service in light and dark: paste, drop and the picker make chips, a text paste keeps its default, the send button waits for uploads, a failed upload shows its reason and can be removed, an unsent upload's removal calls the remove route, the message body names the uploaded IDs, sent images open the lightbox and file chips download under their name, and every `<img>` is a blob URL).
- 2026-09-17: step 8 done — "Retry or edit a sent message from the transcript" (`EntryView.tsx` actions, `Conversation.tsx` retry/edit, `drafts.ts` `resendAttempt`/`canResend` with tests, `Attachments.tsx` reused chips, `claimAttachments` accepts re-sent IDs; no new chunk, main chunk 479.7 → 481.8 kB; verified in a browser with a stubbed service in light and dark: hover and failed-delivery visibility, retry posts the same text with a new ID, edit prefills text and both attachment chips with the image preview and posts the edited text with the same attachment IDs, a non-empty draft asks before being replaced, reused chips never call the remove route, buttons disable while queued/archived and stay enabled while running).
- 2026-09-17: step 9 done — "Copy a message as markdown and export a chat" (`export.ts`, `export.test.ts`, `ExportDialog.tsx`, `useCopy.ts`, `MessageActions` in `EntryView.tsx`; no new chunk, main chunk 481.8 → 486.8 kB; verified in a browser with a throwaway probe in light and dark: the copy button appears on hover under user and agent messages and always under a failed one, copies the markdown source and is disabled while streaming, the dialog downloads a `.md` with the expected headings and a `.json` with the activity entries when asked, and Escape closes it).
- 2026-09-17: step 10 done — "Search within a chat and across chats" (`search.ts`, `search.test.ts`, `FindBar.tsx`, `SearchPalette.tsx`; no new chunk, main chunk 486.8 → 498.7 kB; verified in a browser with a throwaway probe in light and dark: ⌘K lists recent chats, "build" finds titles, messages and tool output across live and archived chats, choosing a tool-output row switches chat, opens the collapsed group and shows "5 of 7" with the current match highlighted, Enter / Shift+Enter step and wrap, typing a new query scrolls to its first match, a streamed message grows the count from 7 to 10 without moving the current match, "resume" finds "résumé", "No matches" shows in the danger colour, Escape clears the highlights and returns focus to the composer, ⌘F and the header button open an empty bar, no console errors).
- 2026-09-17: step 11 done — "Show where the reader left off and a way back to the end" (`transcript.ts`, `transcript.test.ts`, jump button and unread divider in `Conversation.tsx`; no new chunk, main chunk 498.7 → 501.2 kB; verified in a browser with a throwaway probe (removed, not committed) in light and dark: no divider or button on a first visit, scrolling up shows "Jump to latest" which becomes "2 new messages" after two messages and a tool step arrive, a short unread stretch opens at the end with the divider in view, a long one opens with the divider 12 px below the top and "10 new messages" on the button, the jump lands at the end and hides the button, the seen mark is written only while the tab is visible and the reader is at the end, the divider stays for the visit, the divider has no text node, no console errors).
- 2026-09-17: step 11 fix after review — "Fix the unread divider chasing new entries": the divider's index was recomputed against the live entries on every render, so on a return visit to a fully-read chat it appeared above the reader's own next message; the divider's entry ID is now fixed in the mount-time `useState` (`unreadEntry`) and looked up per render (`unreadIndex`), with tests for entries appended after a fully-seen open. A scrolling key, a press on the scrollbar or `scrollend` now interrupts a smooth jump like a wheel or touch. Main chunk 501.2 → 501.7 kB.
- 2026-09-17: step 11 second fix after review — "Keep the seen mark where the reader landed": opening a chat at the divider wrote the seen mark to the last entry in the first commit (the mount layout effect turned following off, but the seen effect of the same commit still closed over `away === null`), so a reader who left without jumping lost the divider on the next visit; the effect now checks the `follow` ref. Scrolling keys typed into an editable element no longer interrupt a smooth jump. Reproduced and confirmed with a throwaway node probe (react-dom on a stub container: before, `layout: setFollow(false) -> passive: away=null -> WRITES SEEN`; after, no write). Main chunk 501.7 → 501.8 kB.
- 2026-09-17: step 12 done — "Show where an agent's link really goes" (`links.ts`, `links.test.ts`, `AgentLink` in `RichText.tsx`; no new chunk, main chunk 501.8 → 502.9 kB; verified in a browser with a throwaway probe (removed, not committed) in light and dark: every assistant http(s) link carries the glyph, `target="_blank"`, `rel="noopener noreferrer"` and the normalised URL as its title, a label claiming `apple.com` on an `evil.example` link shows "→ evil.example", a Cyrillic lookalike and a punycode host show "→ xn--pple-43d.com", a userinfo URL links to `https://evil.example/` with the credentials gone, `mailto:` and `javascript:` stay text, a workspace path still goes to `onFile`, a `v1.2.3` label and a bare autolink get no hint, the user message's links are unchanged, no `<img>` and no off-origin request, no console errors).
- 2026-09-17: step 13 done — "Show what each agent turn took" (`conversation/model.go` `Turn`/`Usage`, `engine.go` turn records and `thread/tokenUsage/updated`, `claude.go` usage from the `result`, `turns.ts`, `turns.test.ts`, `TurnStats.tsx`; no new chunk, main chunk 502.9 → 506.0 kB; verified in a browser with a throwaway probe (removed, not committed) in light and dark: a finished turn shows "1m 12s · 13k tokens (10k in, 3.2k out) · $0.04" beside the copy button with the cache and reasoning counts in its title, a Codex turn shows no cost, the running turn's line counts up under the tool-step group and moves under the message once one streams, tokens reported mid-turn appear while it counts, the line settles when the turn ends, a turn from before records were kept shows nothing, the separators are CSS content, no console errors).
- 2026-09-17: step 14 done — "Complete slash commands and workspace paths in the composer" (`composer.ts`, `composer.test.ts`, `Suggest.tsx`, `chats/paths.go`, `sandbox/paths.go` with `paths_test.go` for the script; no new chunk, main chunk 506.0 → 513.7 kB; verified in a browser with a throwaway probe (removed, not committed) in light and dark: `/` lists the four commands with Stop disabled while idle and Model disabled while running, ↑↓ skip disabled rows, Enter and Tab pick, `/mod` + Tab becomes `/model ` with the provider's models and `5.5` narrows to GPT-5.5 whose pick sets the model and clears the line, `/` + Enter on Export opens the dialog, Stop posts `chats/{id}/stop`, Escape closes the list until the trigger changes and `/stop` sent with ⌘Enter still stops instead of posting a message; `@` lists the workspace root, typing `src/` costs one debounced request, a directory pick keeps completing after its slash, a file pick ends with a space, `@conv` finds files two folders down, a mouse pick keeps the focus, a stopped sandbox shows its reason and the next keystroke retries, moving the caret into a mention completes the part before it, no console errors).
