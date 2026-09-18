# Mermaid rendering fixes

Status: started 2026-09-18 on branch `fix/mermaid-rendering` from origin/main 863acb8.

## Objective

Three problems the owner hit with diagrams in the web chat, diagnosed
against the real chat content (the "does it work" chat on the GKE
deployment):

1. Asked for diagrams, the agent wrote a web page to show them instead of
   putting ```` ```mermaid ```` fences in its reply: nothing tells it the
   chat renders Markdown, diagrams, diffs, math or workspace images.
2. A sequence diagram showed as source with no visible reason. The
   diagram had a real syntax error (`;` inside a message ends the
   statement in Mermaid's sequence grammar), and the error line under
   the block was only Mermaid's first line, "Parse error on line 18:",
   with the snippet, caret and "Expecting …, got 'NEWLINE'" dropped.
3. White boxes with white text in the dark scheme: the agent's
   `classDef trusted fill:#e8f0fe` (and any `style … fill:` in a light
   colour) keeps the theme's light label text, so labels vanish.

## What exists

- `chat/web/src/mermaid.ts` (pure helpers: fence detection, theme
  variables from the design tokens, the image/math refusals, the SVG
  second pass `scrub`, `errorLine`) and `chat/web/src/components/Mermaid.tsx`
  (lazy Mermaid, the render queue, `useMermaid`, `MermaidDiagram`);
  `CodeBlock.tsx` shows the diagram or the source and `diagram.error` as
  `.code-error`. Tests in `mermaid.test.ts` run without a DOM, against
  fake elements.
- The agent's prompt: `sandbox.WardenSystemPrompt` (Claude,
  `--append-system-prompt`, `sandbox/runtime.go`) and the Codex
  `developerInstructions` literal in `chats/engine.go`.

## Steps

1. Prompt: one sentence, shared by both providers, saying the chat renders
   Markdown with ```` ```mermaid ```` diagrams, highlighted and ```` ```diff ````
   fences, `$$` math and `![alt](path)` workspace images, so a diagram goes
   in the reply, not in a file or page.
2. Errors: `errorLine` keeps the line number, the snippet and what was
   expected/got in one line; the full message is the line's title; a
   warning glyph and the fence label "not rendered" make it visible.
3. Contrast: after the SVG mounts, every label is checked against the
   shape behind it (geometry, so it is diagram-agnostic) and its fill is
   switched to the dark or light ink when the contrast is below 3:1.
   Pure parts (`contrastRatio`, `inkFor`, `contrastFixes`) in `mermaid.ts`
   with tests; the DOM walk in `Mermaid.tsx`.
4. Verify in a browser probe against Mermaid 11.17.2 in both schemes, with
   the owner's own diagrams; feature map; deploy locally.

## Key decisions

1. The contrast fix is geometric (the shape whose box contains the label's
   centre, smallest first) rather than per diagram type, because every
   diagram type places labels differently and the agent's styling can
   land on any of them.
2. The agent's own `color:` in a `classDef`/`style` is kept when it reads
   (the pass only touches labels below 3:1), so a deliberately styled
   diagram is not repainted.

## Progress log

- 2026-09-18: diagnosed (see Objective); worktree opened.
- 2026-09-18: steps 1–4 done. `sandbox.ChatRenderingPrompt` appended to
  `WardenSystemPrompt` and to Codex's `developerInstructions`
  (`sandbox`/`chats` tests green). `errorLine` rewritten with tests
  (the owner's sequence error now reads `Parse error on line 18 at
  "...d; metadata audited)": got 'NEWLINE', expecting '()', …`);
  `CodeBlock` shows a warning glyph, the full message as the title and
  "mermaid · not rendered" in the label. Contrast pass: `parseColor`,
  `over`, `contrastRatio`, `inkFor`, `backdrop`, `readableInk` in
  `mermaid.ts` (tests), `fixContrast` in `Mermaid.tsx` run from a
  `useLayoutEffect` after the SVG mounts. Verified in a Vite probe page
  rendering the owner's four diagrams and styled flowchart/state cases
  through the real components against Mermaid 11.17.2, both schemes:
  the `classDef trusted/hostile` boxes read (52 tspans repainted in
  dark, none in light), the theme's own diagrams untouched (0 repaints),
  `fill:#222` under the light scheme repainted light. Web suite 271
  tests, `tsc`, `vite build` green. Not yet merged or deployed.
- 2026-09-18: merged to main as a274dce (fast-forward; full Go suite, web build and 271 tests green). Not deployed: the owner asked to leave the GKE cluster alone; `~/.warden/release` also still runs the old build.
