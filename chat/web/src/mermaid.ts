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

/* Attributes that reference something outside the document. */
export const REF_ATTRS = new Set([
  "href",
  "xlink:href",
  "src",
  "srcset",
  "ping",
  "formaction",
  "action",
  "data",
]);

/* Mermaid runs DOMPurify over label text before it draws, and this is the
   profile it uses. Its default keeps `<img src>`, and a `$$…$$` label is
   drawn through KaTeX as HTML in the live document even with `htmlLabels`
   off, so the fetching tags and referencing attributes are forbidden there
   too; the fence cannot change this because `dompurifyConfig` is secure. */
export const LABEL_PURIFY = {
  FORBID_TAGS: [...DROP_TAGS, "style"],
  FORBID_ATTR: [...REF_ATTRS, "style"],
};

/* A label with `$$` goes through Mermaid's KaTeX path (see LABEL_PURIFY),
   which ignores `htmlLabels` and renders HTML into the live document while
   it measures; a fence carrying one is refused before Mermaid loads. */
export function hasMathLabels(text: string): boolean {
  return text.includes("$$");
}

/* A `url()` that is not a same-document fragment (`url(#marker)`), an
   `@import`, or one of the CSS image functions that take a bare string
   (`image-set("https://…" 1x)`, `image("…")`, `src("…")`); all would fetch. */
const remoteCSS =
  /@import|\burl\((?!\s*['"]?\s*#)|\bimage-set\(|\bimage\(|\bsrc\(|\bcross-fade\(/i;

/* Event handlers, references outside the document (`href`, `src`, ...) and
   attribute values with a remote `url()` are all stripped. */
export function unsafeAttribute(name: string, value: string): boolean {
  const key = name.toLowerCase();
  if (key.startsWith("on")) return true;
  if (REF_ATTRS.has(key)) return !value.trim().startsWith("#");
  return remoteCSS.test(value);
}

/* A `<style>` the diagram carries must not reach out either. */
export function unsafeCSS(text: string): boolean {
  return remoteCSS.test(text);
}

/* The slice of an element the second pass touches, so the walk can be
   exercised on a hand-built tree where no DOM is available. */
export type Scrubbable = {
  localName: string;
  textContent: string | null;
  attributes: ArrayLike<{ name: string; value: string }>;
  remove(): void;
  removeAttribute(name: string): void;
};

/* The second pass itself: drops the elements in DROP_TAGS and any `<style>`
   that fetches, and strips the unsafe attributes from what remains. */
export function scrub(elements: Iterable<Scrubbable>): void {
  for (const el of elements) {
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
}

/* The message of a render error, for the error line's title. */
export function errorText(err: unknown): string {
  return err instanceof Error
    ? err.message
    : typeof err === "string"
      ? err
      : "";
}

/* Mermaid's parser errors span several lines:

     Parse error on line 18:
     ...d; metadata audited)
     -----------------------^
     Expecting 'SOLID_ARROW', ..., got 'NEWLINE'

   The one line under the code block keeps what locates the mistake: the
   line number, the source excerpt the caret points into, and what the
   parser got and expected (cut when the token list runs long). A message
   without that shape is its first line. */
export function errorLine(err: unknown): string {
  const lines = errorText(err)
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);
  const [head, ...rest] = lines;
  if (!head) return "Diagram could not be rendered";
  const parts = [head.replace(/[.:]$/, "")];
  const caret = rest.findIndex((l) => /^-*\^$/.test(l));
  if (caret > 0) parts[0] += ` at "${rest[caret - 1]}"`;
  const expecting = rest.find((l) => l.startsWith("Expecting "));
  if (expecting) {
    const got = /,?\s*got\s+('[^']*'|\S+)$/.exec(expecting);
    const expected = expecting
      .slice("Expecting ".length, got?.index)
      .replace(/,\s*$/, "");
    parts.push(
      [got && `got ${got[1]}`, expected && `expecting ${expected}`]
        .filter(Boolean)
        .join(", "),
    );
  }
  const line = parts.join(": ");
  return line.length > 200 ? `${line.slice(0, 199)}…` : line;
}

/* A colour as Mermaid's SVG carries it and the browser computes it. */
export type RGBA = { r: number; g: number; b: number; a: number };

/* `rgb()`/`rgba()` (what getComputedStyle returns), `#rgb[a]`/`#rrggbb[aa]`,
   `transparent`; anything else (`none`, a `url()` paint, a name) is
   undefined, and a colour with no alpha left is undefined too, since it
   paints nothing. */
export function parseColor(value: string): RGBA | undefined {
  const v = value.trim().toLowerCase();
  if (v === "transparent") return undefined;
  const fn =
    /^rgba?\(\s*([\d.]+)[,\s]+([\d.]+)[,\s]+([\d.]+)(?:\s*[,/]\s*([\d.]+%?))?\s*\)$/.exec(
      v,
    );
  let c: RGBA | undefined;
  if (fn) {
    const alpha = fn[4] === undefined ? 1 : parseAlpha(fn[4]);
    c = { r: +fn[1], g: +fn[2], b: +fn[3], a: alpha };
  } else {
    const hex = /^#([0-9a-f]{3,4}|[0-9a-f]{6}|[0-9a-f]{8})$/.exec(v);
    if (!hex) return undefined;
    let h = hex[1];
    if (h.length <= 4)
      h = h
        .split("")
        .map((ch) => ch + ch)
        .join("");
    const n = (i: number) => parseInt(h.slice(i, i + 2), 16);
    c = { r: n(0), g: n(2), b: n(4), a: h.length === 8 ? n(6) / 255 : 1 };
  }
  return c.a > 0 ? c : undefined;
}

function parseAlpha(s: string): number {
  return s.endsWith("%") ? parseFloat(s) / 100 : parseFloat(s);
}

/* `fg` painted over an opaque `bg`. */
export function over(fg: RGBA, bg: RGBA): RGBA {
  const a = fg.a;
  return {
    r: fg.r * a + bg.r * (1 - a),
    g: fg.g * a + bg.g * (1 - a),
    b: fg.b * a + bg.b * (1 - a),
    a: 1,
  };
}

/* WCAG relative luminance and contrast ratio, on opaque colours. */
export function luminance({ r, g, b }: RGBA): number {
  const lin = (v: number) => {
    const s = v / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

export function contrastRatio(a: RGBA, b: RGBA): number {
  const la = luminance(a);
  const lb = luminance(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

/* The two inks a label can be repainted in: the `--text` token of each
   scheme, so a repainted label matches the labels the theme paints. */
export const INK = { dark: "#1c1b18", light: "#ecebe6" } as const;

/* The ink that reads better on `bg`. */
export function inkFor(bg: RGBA): string {
  const dark = parseColor(INK.dark)!;
  const light = parseColor(INK.light)!;
  return contrastRatio(dark, bg) >= contrastRatio(light, bg)
    ? INK.dark
    : INK.light;
}

/* Below this ratio a label is repainted: WCAG's floor for large text,
   since the theme's own pairs sit well above it and an agent's `style`
   fill that fails it is unreadable rather than merely light. */
export const MIN_CONTRAST = 3;

export type Box = { left: number; top: number; width: number; height: number };

/* What the pass needs of a drawn element: where it is and what colour it
   shows, the shape's fill already composited to opaque. */
export type Label = { box: Box; color: RGBA | undefined };
export type Shape = { box: Box; fill: RGBA };

/* The colour behind a label: the smallest shape whose box holds the
   label's centre (a node over its cluster, an edge label's backing over
   the node behind it), else the diagram's surface. */
export function backdrop(label: Box, shapes: Shape[], surface: RGBA): RGBA {
  const cx = label.left + label.width / 2;
  const cy = label.top + label.height / 2;
  let best: Shape | undefined;
  for (const s of shapes) {
    const { left, top, width, height } = s.box;
    if (cx < left || cx > left + width || cy < top || cy > top + height)
      continue;
    if (!best || width * height < best.box.width * best.box.height) best = s;
  }
  return best?.fill ?? surface;
}

/* The ink a label must be repainted in to read against what is behind
   it, or undefined when it reads as it is. An agent's `style`/`classDef`
   fill in a light colour keeps the theme's light label in the dark
   scheme (and the reverse), which is the case this catches; a label whose
   colour the agent set to something that reads is left alone. */
export function readableInk(
  label: Label,
  shapes: Shape[],
  surface: RGBA,
): string | undefined {
  if (!label.color || !label.box.width || !label.box.height) return undefined;
  const bg = backdrop(label.box, shapes, surface);
  if (contrastRatio(over(label.color, bg), bg) >= MIN_CONTRAST)
    return undefined;
  return inkFor(bg);
}
