import { useEffect, useLayoutEffect, useRef, useState } from "react";
import {
  LABEL_PURIFY,
  THEME_TOKENS,
  errorLine,
  errorText,
  hasImageNodes,
  hasMathLabels,
  parseColor,
  over,
  readableInk,
  scrub,
  themeVariables,
  type DiagramDB,
  type RGBA,
  type Shape,
  type ThemeTokens,
} from "../mermaid";

type Mermaid = typeof import("mermaid").default;

// Mermaid is a lazy chunk loaded the first time a closed ```mermaid fence
// needs it. Renders go through one queue because `initialize` is global and
// a diagram must be drawn with the theme that was set for it.
let loading: Promise<Mermaid> | undefined;
let queue: Promise<unknown> = Promise.resolve();
let seq = 0;

function loadMermaid() {
  return (loading ??= import("mermaid").then((m) => m.default));
}

function readTokens(): ThemeTokens {
  const style = getComputedStyle(document.documentElement);
  const tokens: ThemeTokens = {};
  for (const name of THEME_TOKENS) tokens[name] = style.getPropertyValue(name);
  return tokens;
}

function render(text: string, dark: boolean): Promise<string> {
  const job = queue.then(async () => {
    // Mermaid's KaTeX path draws HTML into the live document before any
    // output filter runs, so a fence that would take it is refused here.
    if (hasMathLabels(text))
      throw new Error("Diagrams with math labels are not rendered");
    const mermaid = await loadMermaid();
    const tokens = readTokens();
    mermaid.initialize({
      // The fence is agent text: strict encodes HTML in labels, disables
      // click callbacks and DOMPurify-sanitises the SVG. Labels stay SVG
      // text (no foreignObject HTML), and that choice is locked against a
      // %%{init}%% directive in the fence by listing it as secure.
      securityLevel: "strict",
      startOnLoad: false,
      suppressErrorRendering: true,
      htmlLabels: false,
      secure: [
        "secure",
        "securityLevel",
        "startOnLoad",
        "maxTextSize",
        "suppressErrorRendering",
        "maxEdges",
        "htmlLabels",
        "theme",
        "themeVariables",
        "themeCSS",
        "fontFamily",
        "altFontFamily",
        "dompurifyConfig",
      ],
      dompurifyConfig: LABEL_PURIFY,
      theme: "base",
      themeVariables: themeVariables(tokens, dark),
      fontFamily: tokens["--font"]?.trim() || undefined,
    });
    const parsed = await mermaid.mermaidAPI.getDiagramFromText(text);
    if (hasImageNodes(parsed.db as DiagramDB))
      throw new Error("Diagrams with images are not rendered");
    const { svg } = await mermaid.render(`mermaid-${++seq}`, text);
    return sanitize(svg);
  });
  queue = job.catch(() => {});
  return job;
}

/* Second pass over Mermaid's output (see `scrub`): parsed in an inert
   document, so nothing here loads or runs while it is inspected. */
function sanitize(svg: string): string {
  const doc = new DOMParser().parseFromString(`<div>${svg}</div>`, "text/html");
  const root = doc.body.firstElementChild;
  if (!root) return "";
  scrub(Array.from(root.querySelectorAll("*")));
  return root.innerHTML;
}

/* Diagram colours are baked into the SVG, so a scheme flip re-renders. */
function useDarkScheme() {
  const [dark, setDark] = useState(
    () => window.matchMedia("(prefers-color-scheme: dark)").matches,
  );
  useEffect(() => {
    const query = window.matchMedia("(prefers-color-scheme: dark)");
    const update = () => setDark(query.matches);
    query.addEventListener("change", update);
    return () => query.removeEventListener("change", update);
  }, []);
  return dark;
}

export type Diagram = {
  svg?: string;
  /* One line saying why the fence stayed as source (`errorLine`). */
  error?: string;
  /* The whole message, for the line's title. */
  detail?: string;
};

/* The rendered SVG (or the parse error) for `text`, once `enabled`; undefined
   while the chunk loads or the render is in flight. */
export function useMermaid(
  text: string,
  enabled: boolean,
): Diagram | undefined {
  const dark = useDarkScheme();
  // A blank fence has nothing to draw; skipping it also spares the chunk.
  const key = enabled && text.trim() ? `${dark}\n${text}` : "";
  const [state, setState] = useState<{ key: string; diagram: Diagram }>();
  useEffect(() => {
    if (!key) return;
    let stopped = false;
    void render(text, dark).then(
      (svg) => {
        if (!stopped) setState({ key, diagram: { svg } });
      },
      (err: unknown) => {
        if (!stopped)
          setState({
            key,
            diagram: { error: errorLine(err), detail: errorText(err) },
          });
      },
    );
    return () => {
      stopped = true;
    };
  }, [key, text, dark]);
  return state?.key === key ? state.diagram : undefined;
}

/* The elements that paint an area a label can sit on. Paths are edges
   (fill none, skipped below) and shapes alike; markers live in <defs>
   and have no box. */
const SHAPES = "rect, path, polygon, circle, ellipse";

/* Labels: SVG text and, should a diagram type ignore `htmlLabels`, the
   leaves of an HTML label; a <text> made of <tspan>s is checked through
   them, since a tspan can carry its own fill. */
const LABELS =
  "text, tspan, foreignObject div, foreignObject span, foreignObject p";

/* The theme paints labels to read on its own fills, but an agent's
   `style`/`classDef` fill can be any colour — a light one under the dark
   scheme's light text leaves the label invisible. So once the SVG is in
   the document, every label is checked against the shape behind it
   (`readableInk`) and repainted in the ink that reads when it fails;
   inline, which the diagram's own stylesheet does not override. */
function fixContrast(root: Element, surface: RGBA): void {
  const shapes: Shape[] = [];
  for (const el of root.querySelectorAll<SVGElement>(SHAPES)) {
    const cs = getComputedStyle(el);
    const fill = parseColor(cs.fill);
    if (!fill) continue;
    const alpha =
      fill.a * Number(cs.fillOpacity || 1) * Number(cs.opacity || 1);
    const box = el.getBoundingClientRect();
    if (!box.width || !box.height || alpha <= 0) continue;
    shapes.push({ box, fill: over({ ...fill, a: alpha }, surface) });
  }
  for (const el of root.querySelectorAll<HTMLElement | SVGElement>(LABELS)) {
    if (el.children.length || !el.textContent?.trim()) continue;
    const html = el instanceof HTMLElement;
    const cs = getComputedStyle(el);
    const ink = readableInk(
      {
        box: el.getBoundingClientRect(),
        color: parseColor(html ? cs.color : cs.fill),
      },
      shapes,
      surface,
    );
    if (!ink) continue;
    if (html) el.style.color = ink;
    else el.style.fill = ink;
  }
}

/* The SVG came out of Mermaid's strict mode, which sanitises it; nothing
   else in the transcript is ever injected as HTML. */
export function MermaidDiagram({ svg }: { svg: string }) {
  const ref = useRef<HTMLDivElement>(null);
  // Before paint, so a repainted label never flashes in its wrong colour.
  // The surface is the code block's (--surface-2, what the theme's
  // `background` was set to), the page's should the token be missing.
  useLayoutEffect(() => {
    const root = ref.current;
    if (!root) return;
    const surface =
      parseColor(readTokens()["--surface-2"] ?? "") ??
      parseColor(getComputedStyle(document.body).backgroundColor);
    if (surface) fixContrast(root, surface);
  }, [svg]);
  return (
    <div
      ref={ref}
      className="mermaid-diagram"
      dangerouslySetInnerHTML={{ __html: svg }}
    />
  );
}
