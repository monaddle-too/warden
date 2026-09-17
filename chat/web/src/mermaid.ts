// Pure helpers behind the Mermaid fence in CodeBlock: which fences are
// diagrams, how the design tokens map onto Mermaid's theme, and how a parse
// failure is shortened to one line.

/* Fences whose info string names Mermaid; `mmd` is Mermaid's file suffix. */
export function isMermaidFence(language: string): boolean {
  return language === "mermaid" || language === "mmd";
}

/* Tokens the diagram theme reads from `:root`, so a diagram follows the
   colour scheme instead of shipping Mermaid's own palette. */
export const THEME_TOKENS = [
  "--font",
  "--surface",
  "--surface-2",
  "--surface-3",
  "--line-strong",
  "--text",
  "--text-2",
  "--accent-soft",
  "--accent-line",
  "--info-soft",
] as const;

export type ThemeTokens = Partial<
  Record<(typeof THEME_TOKENS)[number], string>
>;

/* Mermaid's "base" theme derives every other colour from these, so the
   diagram sits on --surface-2 like a code block does; `darkMode` tells the
   derivation which way to shade. Missing tokens fall back to Mermaid's
   defaults rather than to an empty string, which it would treat as black. */
export function themeVariables(tokens: ThemeTokens, dark: boolean) {
  const vars: Record<string, string | boolean> = { darkMode: dark };
  const put = (name: string, token: keyof ThemeTokens) => {
    const value = tokens[token]?.trim();
    if (value) vars[name] = value;
  };
  put("fontFamily", "--font");
  put("background", "--surface-2");
  put("mainBkg", "--surface-3");
  put("primaryColor", "--surface-3");
  put("primaryTextColor", "--text");
  put("primaryBorderColor", "--line-strong");
  put("lineColor", "--text-2");
  put("textColor", "--text");
  put("secondaryColor", "--accent-soft");
  put("secondaryBorderColor", "--accent-line");
  put("tertiaryColor", "--surface");
  put("tertiaryBorderColor", "--line-strong");
  put("noteBkgColor", "--info-soft");
  put("noteTextColor", "--text");
  put("noteBorderColor", "--line-strong");
  return vars;
}

/* The slice of a parsed diagram's database that can name an image: the
   flowchart vertices, and the layout nodes the unified renderers build. */
export type DiagramDB = {
  getVertices?: () =>
    | Iterable<[string, { img?: unknown }]>
    | Record<string, { img?: unknown }>;
  getData?: () => { nodes?: { img?: unknown }[] } | undefined;
};

/* A flowchart image node (`A@{ img: "..." }`) makes Mermaid fetch the
   picture while it draws, before any output could be filtered, so a diagram
   is parsed and inspected first and refused when it has one. */
export function hasImageNodes(db: DiagramDB): boolean {
  const vertices = db.getVertices?.();
  const list = !vertices
    ? []
    : Symbol.iterator in Object(vertices)
      ? Array.from(vertices as Iterable<[string, { img?: unknown }]>).map(
          ([, v]) => v,
        )
      : Object.values(vertices as Record<string, { img?: unknown }>);
  if (list.some((v) => v?.img)) return true;
  let nodes: { img?: unknown }[] | undefined;
  try {
    nodes = db.getData?.()?.nodes;
  } catch {
    // Some databases can only build layout data after a render; the vertex
    // check above already covers the diagrams that carry images.
  }
  return !!nodes?.some((n) => n?.img);
}

/* Mermaid's strict mode encodes HTML in labels and DOMPurify-sanitises the
   SVG, but it still lets through elements that fetch on their own (`<img>`
   inside a label, `<image>`), `click` links and `url()` fills, and any of
   those would let the agent reach the network or navigate the owner. The
   rendered SVG therefore gets a second pass: these elements are dropped
   and these attributes stripped before the SVG is injected. */
export const DROP_TAGS = new Set([
  "img",
  "image",
  "picture",
  "source",
  "video",
  "audio",
  "track",
  "iframe",
  "frame",
  "object",
  "embed",
  "script",
  "link",
  "meta",
  "base",
  "form",
  "input",
  "button",
  "textarea",
  "select",
]);

/* A `url()` that is not a same-document fragment (`url(#marker)`), or an
   `@import`; both would fetch. */
const remoteCSS = /@import|url\((?!\s*['"]?\s*#)/i;

/* Event handlers, references outside the document (`href`, `src`, ...) and
   attribute values with a remote `url()` are all stripped. */
export function unsafeAttribute(name: string, value: string): boolean {
  const key = name.toLowerCase();
  if (key.startsWith("on")) return true;
  if (
    key === "href" ||
    key === "xlink:href" ||
    key === "src" ||
    key === "srcset" ||
    key === "ping" ||
    key === "formaction" ||
    key === "action" ||
    key === "data"
  )
    return !value.trim().startsWith("#");
  return remoteCSS.test(value);
}

/* A `<style>` the diagram carries must not reach out either. */
export function unsafeCSS(text: string): boolean {
  return remoteCSS.test(text);
}

/* Mermaid's errors span several lines ("Parse error on line 3:\n...^\nExpecting
   ..."); the first line is what fits under the code block. */
export function errorLine(err: unknown): string {
  const message =
    err instanceof Error ? err.message : typeof err === "string" ? err : "";
  const line = message.split("\n").find((l) => l.trim()) ?? "";
  const trimmed = line.trim();
  if (!trimmed) return "Diagram could not be rendered";
  return trimmed.length > 160 ? `${trimmed.slice(0, 159)}…` : trimmed;
}
