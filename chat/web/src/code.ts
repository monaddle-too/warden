// Pure helpers behind the fenced code block in RichText. They work on the
// hast node react-markdown hands the `pre` override, so the block can read
// its own language and text without a second parse of the markdown.
export type HastNode = {
  type: string;
  tagName?: string;
  value?: string;
  properties?: Record<string, unknown>;
  children?: HastNode[];
};

/* Blocks longer than this start collapsed. */
export const COLLAPSE_LINES = 40;

/* The <code> child that carries the fence's language class and text. */
export function codeChild(pre: HastNode | undefined): HastNode | undefined {
  return pre?.children?.find(
    (c) => c.type === "element" && c.tagName === "code",
  );
}

/* Text of a node, whether the highlighter has split it into spans or not. */
export function hastText(node: HastNode | undefined): string {
  if (!node) return "";
  if (node.type === "text") return node.value ?? "";
  return (node.children ?? []).map(hastText).join("");
}

/* Language from the fence's info string, as remark records it in the class
   list (`language-ts`); lower-cased for the label and the highlighter. */
export function codeLanguage(className: unknown): string {
  const list = Array.isArray(className)
    ? className
    : typeof className === "string"
      ? className.split(/\s+/)
      : [];
  for (const entry of list) {
    const value = String(entry);
    const name = value.startsWith("language-")
      ? value.slice(9)
      : value.startsWith("lang-")
        ? value.slice(5)
        : "";
    if (name) return name.toLowerCase();
  }
  return "";
}

/* Lines as the reader counts them: the trailing newline of a fence does
   not add an empty line. */
export function lineCount(text: string): number {
  if (!text) return 0;
  const body = text.endsWith("\n") ? text.slice(0, -1) : text;
  return body.split("\n").length;
}

/* Only a fence with an info string needs the highlighter, so plain prose,
   bare ``` blocks and the fences with their own renderer (Mermaid, diff,
   math) never pay for the lazy chunk. */
export function needsHighlighter(markdown: string): boolean {
  return /^ {0,3}(`{3,}|~{3,}) *(?!(?:mermaid|mmd|diff|patch|math)\b)[A-Za-z]/im.test(
    markdown,
  );
}
