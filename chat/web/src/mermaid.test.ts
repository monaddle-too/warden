import { describe, it, expect } from "vitest";
import {
  DROP_TAGS,
  LABEL_PURIFY,
  INK,
  backdrop,
  contrastRatio,
  errorLine,
  errorText,
  hasImageNodes,
  hasMathLabels,
  inkFor,
  isMermaidFence,
  over,
  parseColor,
  readableInk,
  scrub,
  themeVariables,
  unsafeAttribute,
  unsafeCSS,
  type Scrubbable,
} from "./mermaid";

/* A stand-in for a DOM element: vitest runs here without a DOM, and the
   second pass only needs these members of one. */
function fake(
  localName: string,
  attrs: Record<string, string> = {},
  textContent: string | null = null,
) {
  const el = {
    localName,
    textContent,
    attributes: [] as { name: string; value: string }[],
    removed: false,
    remove() {
      this.removed = true;
    },
    removeAttribute(name: string) {
      this.attributes = this.attributes.filter((a) => a.name !== name);
    },
  };
  el.attributes = Object.entries(attrs).map(([name, value]) => ({
    name,
    value,
  }));
  return el satisfies Scrubbable;
}

describe("mermaid fence helpers", () => {
  it("recognises the mermaid info strings", () => {
    expect(isMermaidFence("mermaid")).toBe(true);
    expect(isMermaidFence("mmd")).toBe(true);
    expect(isMermaidFence("ts")).toBe(false);
    expect(isMermaidFence("")).toBe(false);
  });
  it("maps the design tokens onto the base theme", () => {
    const vars = themeVariables(
      { "--surface-2": " #f3f1ec", "--text": "#1c1b18", "--font": "" },
      false,
    );
    expect(vars.darkMode).toBe(false);
    expect(vars.background).toBe("#f3f1ec");
    expect(vars.primaryTextColor).toBe("#1c1b18");
    // A missing or blank token is left to Mermaid's own default rather than
    // passed as "", which it would read as black.
    expect(vars).not.toHaveProperty("fontFamily");
    expect(vars).not.toHaveProperty("lineColor");
    expect(themeVariables({}, true)).toEqual({ darkMode: true });
  });
  it("drops what could fetch or navigate from the rendered SVG", () => {
    expect(DROP_TAGS.has("img")).toBe(true);
    expect(DROP_TAGS.has("image")).toBe(true);
    expect(DROP_TAGS.has("g")).toBe(false);
    expect(DROP_TAGS.has("foreignobject")).toBe(false);
    expect(unsafeAttribute("onclick", "x()")).toBe(true);
    expect(unsafeAttribute("href", "https://example.com")).toBe(true);
    expect(unsafeAttribute("xlink:href", "#mermaid-1-A")).toBe(false);
    expect(unsafeAttribute("src", "x")).toBe(true);
    // Markers reference the diagram's own defs; a remote fill would fetch.
    expect(unsafeAttribute("marker-end", "url(#mermaid-1_pointEnd)")).toBe(
      false,
    );
    expect(unsafeAttribute("style", "fill:url( '#grad' )")).toBe(false);
    expect(unsafeAttribute("style", "fill:url(https://x/y.svg)")).toBe(true);
    expect(unsafeAttribute("style", 'fill: url("//x/y")')).toBe(true);
    expect(unsafeAttribute("class", "node default")).toBe(false);
    expect(unsafeCSS("#m .edge { stroke: #333; marker-end: url(#a); }")).toBe(
      false,
    );
    expect(unsafeCSS("@import url(https://x/a.css);")).toBe(true);
    expect(unsafeCSS(".n { background: url(https://x/a.png) }")).toBe(true);
    // The image functions that take a bare string fetch just like url().
    expect(unsafeCSS('.n { cursor: image-set("https://x/a.png" 1x) }')).toBe(
      true,
    );
    expect(
      unsafeAttribute("style", "mask-image:-webkit-image-set('//x')"),
    ).toBe(true);
    expect(unsafeAttribute("style", 'background:image("https://x/a")')).toBe(
      true,
    );
    expect(unsafeAttribute("style", "content:src('https://x/a')")).toBe(true);
    expect(unsafeAttribute("style", "font-family:'Curl(ish)'")).toBe(false);
  });
  it("walks a tree dropping fetchers and stripping unsafe attributes", () => {
    const g = fake("g", { class: "node", onclick: "x()" });
    const img = fake("img", { src: "https://x/a.png" });
    const path = fake("path", {
      "marker-end": "url(#m)",
      style: "fill:url(https://x/a.svg)",
    });
    const okStyle = fake("style", {}, "#m .edge { marker-end: url(#a); }");
    const badStyle = fake("style", {}, "@import url(https://x/a.css);");
    const a = fake("a", { "xlink:href": "https://x", href: "#here" });
    scrub([g, img, path, okStyle, badStyle, a]);
    expect(g.removed).toBe(false);
    expect(g.attributes.map((x) => x.name)).toEqual(["class"]);
    expect(img.removed).toBe(true);
    expect(path.attributes.map((x) => x.name)).toEqual(["marker-end"]);
    expect(okStyle.removed).toBe(false);
    expect(badStyle.removed).toBe(true);
    expect(a.attributes.map((x) => x.name)).toEqual(["href"]);
  });
  it("forbids the same fetchers in Mermaid's own label sanitiser", () => {
    for (const tag of ["img", "image", "iframe", "link", "style"])
      expect(LABEL_PURIFY.FORBID_TAGS).toContain(tag);
    for (const attr of ["src", "href", "xlink:href", "style"])
      expect(LABEL_PURIFY.FORBID_ATTR).toContain(attr);
  });
  it("refuses a fence that would take Mermaid's KaTeX path", () => {
    expect(hasMathLabels("sequenceDiagram\n  A->>B: $$x$$ <img src=x>")).toBe(
      true,
    );
    expect(hasMathLabels('classDiagram\n  class A["$$x$$"]')).toBe(true);
    expect(hasMathLabels("graph TD; A[cost $5]-->B")).toBe(false);
  });
  it("refuses a diagram whose database names an image", () => {
    expect(hasImageNodes({})).toBe(false);
    expect(
      hasImageNodes({ getVertices: () => new Map([["A", { img: "x.png" }]]) }),
    ).toBe(true);
    expect(hasImageNodes({ getVertices: () => ({ A: {}, B: {} }) })).toBe(
      false,
    );
    expect(
      hasImageNodes({ getData: () => ({ nodes: [{}, { img: "x.png" }] }) }),
    ).toBe(true);
    expect(
      hasImageNodes({
        getData: () => {
          throw new Error("not laid out");
        },
      }),
    ).toBe(false);
  });
  it("condenses a parse error to its line, excerpt and expectation", () => {
    // The owner's sequence diagram: `;` ends a statement, so the parser
    // hit the line end where an arrow should be.
    expect(
      errorLine(
        new Error(
          "Parse error on line 18:\n...d; metadata audited)\n-----------------------^\nExpecting '()', 'SOLID_OPEN_ARROW', 'DOTTED_OPEN_ARROW', got 'NEWLINE'",
        ),
      ),
    ).toBe(
      "Parse error on line 18 at \"...d; metadata audited)\": got 'NEWLINE', expecting '()', 'SOLID_OPEN_ARROW', 'DOTTED_OPEN_ARROW'",
    );
    // A lexical error has the excerpt and caret but no expectation.
    expect(
      errorLine(
        new Error(
          "Lexical error on line 3. Unrecognized text.\n...A->>B: hi oops\n----------^",
        ),
      ),
    ).toBe('Lexical error on line 3. Unrecognized text at "...A->>B: hi oops"');
    expect(errorLine("\n  No diagram type detected  ")).toBe(
      "No diagram type detected",
    );
    expect(errorLine(undefined)).toBe("Diagram could not be rendered");
    // A long token list is cut, the location kept.
    const long = errorLine(
      new Error(
        `Parse error on line 2:\nA-->\n----^\nExpecting ${Array.from({ length: 40 }, (_, i) => `'T${i}'`).join(", ")}, got 'EOF'`,
      ),
    );
    expect(long).toHaveLength(200);
    expect(
      long.startsWith(
        "Parse error on line 2 at \"A-->\": got 'EOF', expecting 'T0'",
      ),
    ).toBe(true);
    expect(errorText(new Error("a\nb"))).toBe("a\nb");
    expect(errorText(3)).toBe("");
  });
});

describe("label contrast", () => {
  const box = (left: number, top: number, width = 10, height = 10) => ({
    left,
    top,
    width,
    height,
  });
  it("parses the colours the browser and Mermaid produce", () => {
    expect(parseColor("rgb(236, 235, 230)")).toEqual({
      r: 236,
      g: 235,
      b: 230,
      a: 1,
    });
    expect(parseColor("rgba(0, 0, 0, 0.5)")).toEqual({
      r: 0,
      g: 0,
      b: 0,
      a: 0.5,
    });
    expect(parseColor("rgb(0 0 0 / 50%)")?.a).toBe(0.5);
    expect(parseColor("#fff")).toEqual({ r: 255, g: 255, b: 255, a: 1 });
    expect(parseColor("#E8F0FE")).toEqual({ r: 232, g: 240, b: 254, a: 1 });
    expect(parseColor("#00000080")?.a).toBeCloseTo(0.5, 2);
    // Nothing painted: none, transparent, a zero alpha, a paint server.
    expect(parseColor("none")).toBeUndefined();
    expect(parseColor("transparent")).toBeUndefined();
    expect(parseColor("rgba(0, 0, 0, 0)")).toBeUndefined();
    expect(parseColor("url(#grad)")).toBeUndefined();
  });
  it("measures contrast the WCAG way", () => {
    const black = parseColor("#000")!;
    const white = parseColor("#fff")!;
    expect(contrastRatio(black, white)).toBeCloseTo(21, 5);
    expect(contrastRatio(white, white)).toBe(1);
    // The design tokens' own pairs pass in both schemes.
    expect(
      contrastRatio(parseColor(INK.dark)!, parseColor("#ebe8e1")!),
    ).toBeGreaterThan(4.5);
    expect(
      contrastRatio(parseColor(INK.light)!, parseColor("#2a2825")!),
    ).toBeGreaterThan(4.5);
    expect(over({ r: 0, g: 0, b: 0, a: 0.5 }, white)).toEqual({
      r: 127.5,
      g: 127.5,
      b: 127.5,
      a: 1,
    });
    expect(inkFor(white)).toBe(INK.dark);
    expect(inkFor(black)).toBe(INK.light);
    expect(inkFor(parseColor("#e8f0fe")!)).toBe(INK.dark);
  });
  it("finds the shape behind a label, smallest first", () => {
    const surface = parseColor("#1b1a18")!;
    const cluster = { box: box(0, 0, 100, 100), fill: parseColor("#f5f5f5")! };
    const node = { box: box(10, 10, 30, 20), fill: parseColor("#2a2825")! };
    expect(backdrop(box(15, 12, 20, 10), [cluster, node], surface)).toBe(
      node.fill,
    );
    expect(backdrop(box(60, 60), [cluster, node], surface)).toBe(cluster.fill);
    expect(backdrop(box(200, 200), [cluster, node], surface)).toBe(surface);
  });
  it("repaints only the labels that do not read", () => {
    const surface = parseColor("#1b1a18")!;
    const light = parseColor(INK.light);
    // The owner's diagram in the dark scheme: `classDef trusted
    // fill:#e8f0fe` under the theme's light text.
    const styled = { box: box(0, 0, 100, 40), fill: parseColor("#e8f0fe")! };
    expect(
      readableInk(
        { box: box(40, 15, 20, 10), color: light },
        [styled],
        surface,
      ),
    ).toBe(INK.dark);
    // The theme's own node reads, and so does a label off every shape.
    const themed = { box: box(0, 0, 100, 40), fill: parseColor("#2a2825")! };
    expect(
      readableInk(
        { box: box(40, 15, 20, 10), color: light },
        [themed],
        surface,
      ),
    ).toBeUndefined();
    expect(
      readableInk(
        { box: box(400, 15, 20, 10), color: light },
        [styled],
        surface,
      ),
    ).toBeUndefined();
    // An agent's own `color:` that reads on its fill is kept.
    expect(
      readableInk(
        { box: box(40, 15, 20, 10), color: parseColor("#000") },
        [styled],
        surface,
      ),
    ).toBeUndefined();
    // Nothing to check: no colour, or a label without extent.
    expect(
      readableInk(
        { box: box(40, 15, 20, 10), color: undefined },
        [styled],
        surface,
      ),
    ).toBeUndefined();
    expect(
      readableInk({ box: box(40, 15, 0, 0), color: light }, [styled], surface),
    ).toBeUndefined();
    // A translucent label is judged as painted over its backdrop.
    expect(
      readableInk(
        { box: box(40, 15, 20, 10), color: { r: 0, g: 0, b: 0, a: 0.1 } },
        [styled],
        surface,
      ),
    ).toBe(INK.dark);
  });
});
