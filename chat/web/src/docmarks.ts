/* The inline mark grammar of Google Docs suggestions, mirroring
   policy/docmodel.go: "**" toggles bold, "*" italic, "[text](url)" links,
   backslash escapes. Pure helpers, testable without a browser. */
export type Run = {
  text: string;
  bold: boolean;
  italic: boolean;
  link: string;
};
export function parseInline(text: string): Run[] {
  const runs: Run[] = [];
  let current = "",
    bold = false,
    italic = false,
    link = "";
  const flush = () => {
    if (current) runs.push({ text: current, bold, italic, link });
    current = "";
  };
  for (let i = 0; i < text.length; ) {
    const c = text[i];
    if (c === "\\" && i + 1 < text.length) {
      current += text[i + 1];
      i += 2;
    } else if (c === "*" && text[i + 1] === "*") {
      flush();
      bold = !bold;
      i += 2;
    } else if (c === "*") {
      flush();
      italic = !italic;
      i++;
    } else if (c === "[" && !link) {
      const target = linkTarget(text, i + 1);
      if (target) {
        flush();
        link = target;
        i++;
      } else {
        current += c;
        i++;
      }
    } else if (c === "]" && link && text[i + 1] === "(") {
      const end = text.indexOf(")", i + 2);
      flush();
      link = "";
      i = end + 1;
    } else {
      current += c;
      i++;
    }
  }
  flush();
  return runs;
}
function linkTarget(text: string, from: number): string {
  for (let i = from; i < text.length; i++) {
    if (text[i] === "\\") i++;
    else if (text[i] === "]" && text[i + 1] === "(") {
      const end = text.indexOf(")", i + 2);
      if (end < 0) return "";
      const url = text.slice(i + 2, end);
      return /\s/.test(url) ? "" : url;
    }
  }
  return "";
}
/* Word-level diff of two paragraphs, marks included, for the common case
   of one paragraph edited in place. */
export type Token = Run & { key: string };
function tokenize(text: string): Token[] {
  const out: Token[] = [];
  for (const run of parseInline(text))
    for (const part of run.text.split(/(\s+)/))
      if (part)
        out.push({
          ...run,
          text: part,
          key: JSON.stringify([part, run.bold, run.italic, run.link]),
        });
  return out;
}
export function wordDiff(before: string, after: string) {
  const a = tokenize(before),
    b = tokenize(after);
  const table: number[][] = Array.from({ length: a.length + 1 }, () =>
    new Array(b.length + 1).fill(0),
  );
  for (let i = a.length - 1; i >= 0; i--)
    for (let j = b.length - 1; j >= 0; j--)
      table[i][j] =
        a[i].key === b[j].key
          ? table[i + 1][j + 1] + 1
          : Math.max(table[i + 1][j], table[i][j + 1]);
  const ops: { kind: "=" | "-" | "+"; token: Token }[] = [];
  let i = 0,
    j = 0;
  while (i < a.length && j < b.length) {
    if (a[i].key === b[j].key) (ops.push({ kind: "=", token: a[i++] }), j++);
    else if (table[i + 1][j] >= table[i][j + 1])
      ops.push({ kind: "-", token: a[i++] });
    else ops.push({ kind: "+", token: b[j++] });
  }
  while (i < a.length) ops.push({ kind: "-", token: a[i++] });
  while (j < b.length) ops.push({ kind: "+", token: b[j++] });
  return ops;
}
