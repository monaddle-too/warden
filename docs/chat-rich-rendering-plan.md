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
- Attachments live in the sandbox (the agent reads them like any file)
  rather than in a host-side store, so nothing new needs sharing policy.

## Progress log

- 2026-09-17: step 0 done.
- 2026-09-17: step 1 done — "Render fenced code with highlighting, copy, wrap and collapse" (`CodeBlock.tsx`, `code.ts`; highlighter is a 167 kB lazy chunk, main chunk 454.6 → 458.8 kB).
- 2026-09-17: step 2 done — "Render closed mermaid fences as diagrams" (`Mermaid.tsx`, `mermaid.ts`; Mermaid is lazy chunks, main chunk 458.8 → 463.6 kB; verified in a browser that no agent-controlled URL is fetched).
- 2026-09-17: step 2 fix after review — "Refuse mermaid math labels and lock its label sanitiser": the KaTeX label path fetched before the output filter (confirmed in a browser: 4 beacon requests without the fix, 0 with it); `$$` fences refused, `dompurifyConfig` set, CSS image functions filtered, blank fences skip the chunk, the SVG walk (`scrub`) is pure and unit-tested on a fake tree. Main chunk 463.6 → 463.8 kB.
- 2026-09-17: step 3 done — "Show workspace images inline in the transcript" (`InlineImage.tsx`, `Lightbox.tsx`, `images.ts`, `chats/{id}/image-file`; main chunk 463.8 → 466.4 kB; verified in a browser that remote, `data:` and `..` sources stay as alt text and no request leaves for them).
- 2026-09-17: step 4 done — "Render unified diffs in activity detail and diff fences" (`DiffView.tsx`, `diff.ts`; no lazy chunk needed, main chunk 466.4 → 471.1 kB; verified in a browser in light and dark that an `<img>` in a diff line stays text and nothing is fetched).
- 2026-09-17: step 5 done — "Render math with remark-math and KaTeX" (`math.ts`, `katex.ts`; KaTeX and its stylesheet are lazy chunks of 259 + 11 kB JS and 29 kB CSS, main chunk 471.1 → 471.6 kB; verified in a browser in light and dark that trusted-only commands render as errors and nothing is fetched).
