// Pure helpers behind streaming-safe rendering in RichText. A message that is
// still being written is re-parsed on every chunk, so lines whose reading is
// not settled yet would flicker through their intermediate readings: a table
// header is a paragraph until its delimiter row is complete, a fence opener
// changes language with every character of its info string, and a formula
// renders as an error until its closing `$$` arrives. `displayText` holds
// such tail lines back until the newline that settles them, and `touchesEnd`
// tells a block that reaches the end of the text apart from one that is
// already final, so per-block renderers (Mermaid, KaTeX) can wait for just
// that block rather than for the whole message.

type Point = { offset?: number };
export type Positioned = { position?: { start: Point; end: Point } };
export type MdNode = Positioned & {
  type: string;
  value?: string;
  lang?: string | null;
  children?: MdNode[];
};

/* A block that reaches the end of the text is still being written: an
   unclosed fence or formula runs to the end (CommonMark closes it there),
   and a closed one is only final once its last line has a newline, since the
   next character could still turn ``` into ```x and reopen it. A node without
   a position is treated as open, the safe reading while streaming. */
export function touchesEnd(
  node: Positioned | undefined,
  length: number,
): boolean {
  const end = node?.position?.end.offset;
  return end === undefined || end >= length;
}

// Container prefixes (quotes, list markers) are skipped so a fence or table
// inside a list is seen too; the scan is a heuristic and only decides what is
// held back, never how anything parses.
const prefix = String.raw`^[\s>]*(?:[-*+]\s+|\d+[.)]\s+)?`;
const fenceLine = new RegExp(prefix + "(`{3,}|~{3,})(.*)$");
// A fence opener with its info string still incomplete, or the one or two
// marks that may become one.
const opener = new RegExp(prefix + "(?:`{3,}[^`]*|~{3,}.*|`{1,2}|~{1,2})$");
// A closing fence in progress inside an open fence.
const closer = new RegExp(prefix + "(?:`+|~+)\\s*$");
const pipeRow = new RegExp(prefix + "\\|");
// The delimiter row of a table at any point of being written, from nothing
// at all to `| :--- | ---: |`.
const delimiterRow =
  /^[\s>]*\|?[ \t]*(?::?-*:?[ \t]*(?:\|[ \t]*:?-*:?[ \t]*)*)?\|?[ \t]*$/;

const mathFence = new RegExp(prefix + String.raw`\$\$(.*)$`);

/* What is still open after `lines`: a fence, or a `$$` block outside one
   (a line that is `$$` alone opens it and the next such line closes it;
   `$$x$$` on one line is inline math). */
function openBlocks(lines: string[]): { fence: boolean; math: boolean } {
  let fence: { mark: string; size: number } | undefined;
  let math = false;
  for (const line of lines) {
    const match = fenceLine.exec(line);
    if (match) {
      const [, marks, rest] = match;
      if (!fence) {
        // A backtick fence cannot have backticks in its info string.
        if (marks[0] !== "`" || !rest.includes("`"))
          fence = { mark: marks[0], size: marks.length };
      } else if (
        marks[0] === fence.mark &&
        marks.length >= fence.size &&
        !rest.trim()
      )
        fence = undefined;
      continue;
    }
    if (fence) continue;
    const dollars = mathFence.exec(line);
    if (!dollars) continue;
    if (math) {
      if (!dollars[1].trim()) math = false;
    } else if (!dollars[1].includes("$$")) math = true;
  }
  return { fence: fence !== undefined, math };
}
const fenceOpen = (lines: string[]) => openBlocks(lines).fence;

/* The offset a block must reach to count as open (`touchesEnd`): the end
   of the text, or, when the text ends inside an unterminated fence or
   `$$` block, the end of its last line without the line ending, because
   micromark ends a block inside a blockquote before the newline. A closed
   block followed by a newline still reaches neither. */
export function openFrom(text: string): number {
  const open = openBlocks(text.split("\n"));
  return open.fence || open.math
    ? text.replace(/\s+$/, "").length
    : text.length;
}

/* A pipe line whose meaning depends on the next line: the line before it
   is not a table row, so it becomes a table header if a delimiter row
   follows and stays a paragraph otherwise. */
function headerCandidate(line: string | undefined, before: string | undefined) {
  return line !== undefined && pipeRow.test(line) && !before?.includes("|");
}

/* The text to render while the message streams: the same markdown with the
   trailing lines that could still change their reading held back until the
   newline that settles them. Inside a fence code shows as it arrives and only
   a closing fence in progress waits; outside, an opener waits for its info
   string, a pipe line for its end (so a table row appears whole and a header
   never shows as a paragraph first), and a would-be header for the delimiter
   row that decides it. */
export function displayText(text: string): string {
  const lines = text.split("\n");
  const tail = lines[lines.length - 1];
  const prev = lines[lines.length - 2];
  let hold = 0;
  if (fenceOpen(lines.slice(0, -1))) {
    if (closer.test(tail)) hold = 1;
  } else if (opener.test(tail)) {
    hold = 1;
  } else if (
    delimiterRow.test(tail) &&
    headerCandidate(prev, lines[lines.length - 3])
  ) {
    hold = 2;
  } else if (pipeRow.test(tail)) {
    hold = 1;
  }
  if (!hold) return text;
  const kept = lines.slice(0, lines.length - hold);
  return kept.length ? kept.join("\n") + "\n" : "";
}

/* remark plugin for a streaming message: a formula that reaches the end of
   the text is not finished, so it stays as the text written so far instead
   of rendering as a KaTeX error on every chunk. Runs after remark-math has
   parsed, so the rest of the message keeps its math. An open ```math fence
   (display math through rehype-katex) loses its language for the same
   reason and shows as a plain code block until it closes. */
export function holdOpenMath() {
  return (tree: MdNode, file: { value?: unknown }) => {
    const text = String(file.value ?? "");
    const from = openFrom(text);
    const walk = (node: MdNode) => {
      node.children?.forEach((child, i, children) => {
        if (child.type === "code" && child.lang === "math") {
          if (touchesEnd(child, from)) children[i] = { ...child, lang: null };
        } else if (
          (child.type === "math" || child.type === "inlineMath") &&
          touchesEnd(child, from)
        ) {
          // Display math is rebuilt from its content (the source tail
          // would carry a quote's `> ` prefixes); inline math is one line
          // and shows as written.
          const start = child.position?.start.offset;
          const raw =
            child.type === "math"
              ? `$$\n${child.value ?? ""}`
              : start === undefined
                ? `$${child.value ?? ""}$`
                : text.slice(start);
          children[i] =
            child.type === "math"
              ? { type: "paragraph", children: [{ type: "text", value: raw }] }
              : { type: "text", value: raw };
        } else {
          walk(child);
        }
      });
    };
    walk(tree);
  };
}
