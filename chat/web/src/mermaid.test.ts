import { describe, it, expect } from "vitest";
import {
  DROP_TAGS,
  LABEL_PURIFY,
  errorLine,
  hasImageNodes,
  hasMathLabels,
  isMermaidFence,
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
  it("keeps one line of a parse error", () => {
    expect(
      errorLine(
        new Error(
          "Parse error on line 2:\ngraph TD; A-->\n----------^\nExpecting 'AMP'",
        ),
      ),
    ).toBe("Parse error on line 2:");
    expect(errorLine("\n  No diagram type detected  ")).toBe(
      "No diagram type detected",
    );
    expect(errorLine(undefined)).toBe("Diagram could not be rendered");
    expect(errorLine(new Error("x".repeat(200)))).toHaveLength(160);
  });
});
