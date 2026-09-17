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
| 3 | Inline images in markdown | pending | `![alt](relative/path)` → fetch via `chats/{id}/file`, normalise server-side with imageguard (new `image-file`-style handling or reuse `attachImage` path), show through `ImageAttachment`-like element with lightbox; `http(s)` and `data:` sources stay as alt text |
| 4 | Diff rendering | pending | detect unified diff in activity `detail` and in ```` ```diff ```` fences; +/- line colouring, file header, hunk collapse |
| 5 | Math | pending | `remark-math` + `rehype-katex`, KaTeX CSS lazy-loaded, `trust: false`, `throwOnError: false` |
| 6 | Streaming-safe rendering | pending | while `entry.isStreaming`: close an unterminated fence for display, defer Mermaid/KaTeX; no flicker of half-parsed tables |
| 7 | Composer attachments: paste / drag-drop / picker for images and files | pending | backend: `POST chats/{id}/attachments` (multipart, ≤ 8 MiB) writes the file into the sandbox workspace under `.warden/attachments/<id>.<ext>` via a new worker op; images also go through imageguard and become an `image` entry; the turn input carries the path (and a `localImage` / image content block for image-capable providers); front-end chips in the composer with remove buttons |
| 8 | Edit-and-resend / retry last message | pending | user entry hover actions; retry re-sends the same text with a new ID; edit prefills the composer |
| 9 | Copy message as markdown, export chat (markdown + JSON) | pending | per-message copy button; chat menu "Export…" builds the file client-side from `chat.conversation` |
| 10 | Search: within a chat and across chat titles/entries | pending | ⌘K / Ctrl+K palette in `ChatShell.tsx`; in-chat find highlights matches and scrolls to them; pure matching logic unit-tested |
| 11 | Jump-to-bottom button, unread divider | pending | button appears when `follow` is false; divider at first entry newer than the last-seen timestamp per chat (localStorage) |
| 12 | Safe-link display | pending | agent links show the real host on hover/title and an external-link glyph; punycode/lookalike hosts rendered as ASCII; user links unchanged |
| 13 | Turn timing and token/cost per assistant turn | pending | duration from `createdAt` of the user message to the end of streaming; tokens if the provider stream reports them (check `agent/rpc.go`, `agent/claude.go`), otherwise duration only |
| 14 | Slash commands and `@path` mentions in the composer | pending | `/` opens a small list (stop, model, export, clear draft); `@` completes workspace paths via a new `chats/{id}/paths?q=` route backed by the worker |
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
  before it is injected. A CSP on the served page would be a further
  backstop and is out of this plan's scope.
- Attachments live in the sandbox (the agent reads them like any file)
  rather than in a host-side store, so nothing new needs sharing policy.

## Progress log

- 2026-09-17: step 0 done.
- 2026-09-17: step 1 done — "Render fenced code with highlighting, copy, wrap and collapse" (`CodeBlock.tsx`, `code.ts`; highlighter is a 167 kB lazy chunk, main chunk 454.6 → 458.8 kB).
- 2026-09-17: step 2 done — "Render closed mermaid fences as diagrams" (`Mermaid.tsx`, `mermaid.ts`; Mermaid is lazy chunks, main chunk 458.8 → 463.6 kB; verified in a browser that no agent-controlled URL is fetched).
