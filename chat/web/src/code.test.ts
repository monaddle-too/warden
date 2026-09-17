import { describe, it, expect } from "vitest";
import {
  codeChild,
  codeLanguage,
  hastText,
  lineCount,
  needsHighlighter,
} from "./code";

describe("fenced code helpers", () => {
  it("reads the language from remark's class list", () => {
    expect(codeLanguage(["language-TypeScript"])).toBe("typescript");
    expect(codeLanguage("hljs lang-go")).toBe("go");
    expect(codeLanguage(["hljs"])).toBe("");
    expect(codeLanguage(undefined)).toBe("");
  });
  it("recovers the text after the highlighter has split it into spans", () => {
    const pre = {
      type: "element",
      tagName: "pre",
      children: [
        {
          type: "element",
          tagName: "code",
          properties: { className: ["language-js"] },
          children: [
            {
              type: "element",
              tagName: "span",
              children: [{ type: "text", value: "const" }],
            },
            { type: "text", value: " x = 1;\n" },
          ],
        },
      ],
    };
    expect(hastText(codeChild(pre))).toBe("const x = 1;\n");
    expect(codeChild({ type: "element", tagName: "pre" })).toBeUndefined();
  });
  it("counts lines the way the reader sees them", () => {
    expect(lineCount("")).toBe(0);
    expect(lineCount("one")).toBe(1);
    expect(lineCount("one\ntwo\n")).toBe(2);
    expect(lineCount("\n\n")).toBe(2);
  });
  it("loads the highlighter only for fences with a language", () => {
    expect(needsHighlighter("```ts\nlet a\n```")).toBe(true);
    expect(needsHighlighter("~~~python\n~~~")).toBe(true);
    expect(needsHighlighter("```\nplain\n```")).toBe(false);
    expect(needsHighlighter("inline `code` and ``` in prose")).toBe(false);
    expect(needsHighlighter("    indented\n")).toBe(false);
    // Fences with their own renderer do not need it either.
    expect(needsHighlighter("```diff\n-a\n+b\n```")).toBe(false);
    expect(needsHighlighter("```Mermaid\ngraph TD\n```")).toBe(false);
    expect(needsHighlighter("```diff\n```\n```go\n```")).toBe(true);
  });
});
