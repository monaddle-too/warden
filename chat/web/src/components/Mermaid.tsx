import { useEffect, useState } from "react";
import {
  DROP_TAGS,
  THEME_TOKENS,
  errorLine,
  hasImageNodes,
  themeVariables,
  unsafeAttribute,
  unsafeCSS,
  type DiagramDB,
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

/* Second pass over Mermaid's output (see DROP_TAGS): parsed in an inert
   document, so nothing here loads or runs while it is inspected. */
function sanitize(svg: string): string {
  const doc = new DOMParser().parseFromString(`<div>${svg}</div>`, "text/html");
  const root = doc.body.firstElementChild;
  if (!root) return "";
  for (const el of Array.from(root.querySelectorAll("*"))) {
    const tag = el.localName.toLowerCase();
    if (
      DROP_TAGS.has(tag) ||
      (tag === "style" && unsafeCSS(el.textContent ?? ""))
    ) {
      el.remove();
      continue;
    }
    for (const attr of Array.from(el.attributes))
      if (unsafeAttribute(attr.name, attr.value)) el.removeAttribute(attr.name);
  }
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

export type Diagram = { svg?: string; error?: string };

/* The rendered SVG (or the parse error) for `text`, once `enabled`; undefined
   while the chunk loads or the render is in flight. */
export function useMermaid(
  text: string,
  enabled: boolean,
): Diagram | undefined {
  const dark = useDarkScheme();
  const key = enabled ? `${dark}\n${text}` : "";
  const [state, setState] = useState<{ key: string; diagram: Diagram }>();
  useEffect(() => {
    if (!key) return;
    let stopped = false;
    void render(text, dark).then(
      (svg) => {
        if (!stopped) setState({ key, diagram: { svg } });
      },
      (err: unknown) => {
        if (!stopped) setState({ key, diagram: { error: errorLine(err) } });
      },
    );
    return () => {
      stopped = true;
    };
  }, [key, text, dark]);
  return state?.key === key ? state.diagram : undefined;
}

/* The SVG came out of Mermaid's strict mode, which sanitises it; nothing
   else in the transcript is ever injected as HTML. */
export function MermaidDiagram({ svg }: { svg: string }) {
  return (
    <div
      className="mermaid-diagram"
      dangerouslySetInnerHTML={{ __html: svg }}
    />
  );
}
