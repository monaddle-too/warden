import { describe, it, expect } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import Markdown from "react-markdown";
import remarkMath from "remark-math";
import {
  displayText,
  holdOpenMath,
  touchesEnd,
  type MdNode,
} from "./streaming";

describe("held-back tail lines", () => {
  it("leaves settled text alone", () => {
    expect(displayText("")).toBe("");
    expect(displayText("hello")).toBe("hello");
    expect(displayText("hello\n")).toBe("hello\n");
    expect(displayText("a | b in prose")).toBe("a | b in prose");
    expect(displayText("`git status` shows")).toBe("`git status` shows");
    expect(displayText("- item\n- next")).toBe("- item\n- next");
  });
  it("waits for a fence opener's info string", () => {
    expect(displayText("hi\n``")).toBe("hi\n");
    expect(displayText("hi\n```")).toBe("hi\n");
    expect(displayText("hi\n```ty")).toBe("hi\n");
    expect(displayText("hi\n~~~py")).toBe("hi\n");
    expect(displayText("- ```js")).toBe("");
    expect(displayText("hi\n```ts\n")).toBe("hi\n```ts\n");
  });
  it("shows code as it arrives and waits only for the closing fence", () => {
    expect(displayText("```ts\nconst x")).toBe("```ts\nconst x");
    expect(displayText("```ts\n| a | b |")).toBe("```ts\n| a | b |");
    expect(displayText("```ts\nx\n``")).toBe("```ts\nx\n");
    expect(displayText("```ts\nx\n```")).toBe("```ts\nx\n");
    expect(displayText("```ts\nx\n```\n")).toBe("```ts\nx\n```\n");
    expect(displayText("```ts\nx\n```\nmore")).toBe("```ts\nx\n```\nmore");
    // A longer closer still closes; a shorter one is content.
    expect(displayText("````\n```\nx")).toBe("````\n```\nx");
    expect(displayText("````\n````\n| x")).toBe("````\n````\n");
  });
  it("shows a table header only once its delimiter row is complete", () => {
    expect(displayText("Table:\n| a | b")).toBe("Table:\n");
    expect(displayText("Table:\n| a | b |\n")).toBe("Table:\n");
    expect(displayText("Table:\n| a | b |\n|")).toBe("Table:\n");
    expect(displayText("Table:\n| a | b |\n|---|-")).toBe("Table:\n");
    expect(displayText("Table:\n| a | b |\n| :---: | --:")).toBe("Table:\n");
    expect(displayText("| a | b |\n--|-")).toBe("");
    expect(displayText("Table:\n| a | b |\n|---|---|\n")).toBe(
      "Table:\n| a | b |\n|---|---|\n",
    );
    // A pipe line followed by prose was a paragraph after all.
    expect(displayText("| a | b |\nnot a table")).toBe(
      "| a | b |\nnot a table",
    );
  });
  it("shows a table row whole", () => {
    const head = "| a | b |\n|---|---|\n";
    expect(displayText(head + "| 1 | 2")).toBe(head);
    expect(displayText(head + "| 1 | 2 |")).toBe(head);
    expect(displayText(head + "| 1 | 2 |\n")).toBe(head + "| 1 | 2 |\n");
    expect(displayText(head + "| 1 | 2 |\n|")).toBe(head + "| 1 | 2 |\n");
    expect(displayText("> " + head + "> | 1")).toBe("> " + head);
  });
});

describe("blocks that reach the end of the text", () => {
  const at = (start: number, end: number) => ({
    position: { start: { offset: start }, end: { offset: end } },
  });
  it("tells an open block from a settled one", () => {
    expect(touchesEnd(at(3, 16), 16)).toBe(true);
    expect(touchesEnd(at(3, 20), 21)).toBe(false);
    expect(touchesEnd({}, 5)).toBe(true);
    expect(touchesEnd(undefined, 5)).toBe(true);
  });
});

describe("open math while streaming", () => {
  it("turns a formula at the end of the text back into its source", () => {
    const text = "so $a$ and\n$$\n\\frac{a}{b";
    const tree: MdNode = {
      type: "root",
      children: [
        {
          type: "paragraph",
          children: [
            { type: "text", value: "so " },
            { type: "inlineMath", value: "a", ...at(3, 6) },
            { type: "text", value: " and" },
          ],
        },
        { type: "math", value: "\\frac{a}{b", ...at(11, text.length) },
      ],
    };
    holdOpenMath()(tree, { value: text });
    expect(tree.children?.[0].children?.[1]).toEqual({
      type: "inlineMath",
      value: "a",
      ...at(3, 6),
    });
    expect(tree.children?.[1]).toEqual({
      type: "paragraph",
      children: [{ type: "text", value: "$$\n\\frac{a}{b" }],
    });
  });
  it("falls back to the node's value without positions", () => {
    const tree: MdNode = {
      type: "root",
      children: [
        { type: "paragraph", children: [{ type: "inlineMath", value: "x" }] },
        { type: "math", value: "y" },
      ],
    };
    holdOpenMath()(tree, { value: "" });
    expect(tree.children?.[0].children?.[0]).toEqual({
      type: "text",
      value: "$x$",
    });
    expect(tree.children?.[1].children?.[0].value).toBe("$$\ny");
  });
  it("keeps settled math through the real parser", () => {
    const render = (text: string) =>
      renderToStaticMarkup(
        createElement(
          Markdown,
          { remarkPlugins: [remarkMath, holdOpenMath] },
          text,
        ),
      );
    expect(render("$a$ then\n$$\nb\n$$\nand $c$.")).toBe(
      '<p><code class="language-math math-inline">a</code> then</p>\n' +
        '<pre><code class="language-math math-display">b</code></pre>\n' +
        '<p>and <code class="language-math math-inline">c</code>.</p>',
    );
    expect(render("$a$ then\n$$\nb")).toBe(
      '<p><code class="language-math math-inline">a</code> then</p>\n' +
        "<p>$$\nb</p>",
    );
    expect(render("closed at the end $c$")).toBe(
      "<p>closed at the end $c$</p>",
    );
  });
  function at(start: number, end: number) {
    return { position: { start: { offset: start }, end: { offset: end } } };
  }
});
