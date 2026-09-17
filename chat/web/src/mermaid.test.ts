import { describe, it, expect } from "vitest";
import {
  DROP_TAGS,
  errorLine,
  hasImageNodes,
  isMermaidFence,
  themeVariables,
  unsafeAttribute,
  unsafeCSS,
} from "./mermaid";

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
