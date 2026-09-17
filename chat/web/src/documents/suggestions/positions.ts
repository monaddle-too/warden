/* Mapping between the page and the draft the server keeps: draft paragraph
   numbers (deleted paragraphs excluded), code-point offsets of a paragraph's
   surviving text, and the first position of each suggestion. */
import type { Node as PMNode } from "@tiptap/pm/model";
import type { DocComment } from "./schema";

export type Block = { node: PMNode; pos: number };

function deletedBlock(node: PMNode) {
  const s = node.attrs?.suggestion as { kind?: string } | null;
  return !!s && s.kind === "delete";
}
function deletedText(node: PMNode) {
  return node.marks.some((m) => m.type.name === "suggestDelete");
}

/* The draft's paragraphs, in order, as page blocks. */
export function draftBlocks(doc: PMNode): Block[] {
  const out: Block[] = [];
  doc.descendants((node, pos) => {
    if (node.type.name === "frozenBlock" || node.type.name === "frozenTable") {
      out.push({ node, pos });
      return false;
    }
    if (!node.isTextblock) return true;
    if (deletedBlock(node)) return false;
    if (node.attrs?.synthetic && node.textContent === "") return false;
    out.push({ node, pos });
    return false;
  });
  return out;
}

/* Code points of the surviving text before `pos` inside `block`. */
function offsetAt(block: Block, pos: number) {
  let offset = 0;
  block.node.forEach((child, childOffset) => {
    const start = block.pos + 1 + childOffset;
    if (start >= pos) return;
    if (child.isText && !deletedText(child)) {
      const end = start + child.nodeSize;
      const slice = child.text!.slice(
        0,
        Math.max(0, Math.min(pos, end) - start),
      );
      offset += Array.from(slice).length;
    } else if (child.type.name === "hardBreak") offset += 1;
  });
  return offset;
}

/* A selection as a comment anchor: draft paragraph number, offsets, quote. */
export function selectionAnchor(
  doc: PMNode,
  from: number,
  to: number,
): Pick<DocComment, "paragraph" | "from" | "to" | "quote"> | null {
  const blocks = draftBlocks(doc);
  const index = blocks.findIndex(
    (b) => from >= b.pos && from <= b.pos + b.node.nodeSize,
  );
  if (index < 0) return null;
  const block = blocks[index];
  const end = Math.min(to, block.pos + block.node.nodeSize - 1);
  return {
    paragraph: index + 1,
    from: offsetAt(block, from),
    to: offsetAt(block, end),
    quote: doc.textBetween(from, end, " ").slice(0, 200),
  };
}

/* Page positions of a comment's range, or its whole paragraph. */
export function commentRange(
  doc: PMNode,
  comment: DocComment,
): { from: number; to: number } | null {
  const block = draftBlocks(doc)[comment.paragraph - 1];
  if (!block) return null;
  const start = block.pos + 1,
    end = block.pos + block.node.nodeSize - 1;
  if (
    block.node.isAtom ||
    comment.to === undefined ||
    comment.to <= (comment.from ?? 0)
  )
    return { from: block.pos, to: block.pos + block.node.nodeSize };
  let from = -1,
    to = -1,
    offset = 0;
  block.node.forEach((child, childOffset) => {
    const at = start + childOffset;
    if (child.isText && !deletedText(child)) {
      const points = Array.from(child.text!);
      let cursor = at;
      for (const point of points) {
        if (offset === comment.from) from = cursor;
        offset += 1;
        cursor += point.length;
        if (offset === comment.to) to = cursor;
      }
    } else if (child.type.name === "hardBreak") {
      if (offset === comment.from) from = at;
      offset += 1;
      if (offset === comment.to) to = at + 1;
    }
  });
  if (from < 0 || to < 0 || to <= from) return { from: start, to: end };
  return { from, to };
}

/* The page position of a code-point offset into a draft paragraph's
   surviving text (the caret's home after the page is replaced). */
export function anchorPosition(
  doc: PMNode,
  paragraph: number,
  offset: number,
): number | null {
  const block = draftBlocks(doc)[paragraph - 1];
  if (!block) return null;
  if (block.node.isAtom) return block.pos;
  let count = 0,
    found = -1;
  block.node.forEach((child, childOffset) => {
    if (found >= 0) return;
    const at = block.pos + 1 + childOffset;
    if (child.isText && !deletedText(child)) {
      let cursor = at;
      for (const point of Array.from(child.text!)) {
        if (count === offset) {
          found = cursor;
          return;
        }
        count += 1;
        cursor += point.length;
      }
      if (count === offset) found = cursor;
    } else if (child.type.name === "hardBreak") {
      if (count === offset) found = at;
      count += 1;
    }
  });
  if (found >= 0) return found;
  return block.pos + block.node.nodeSize - 1; // past the end: the paragraph's end
}

/* First page position of each suggestion, for the margin. */
export function suggestionPositions(doc: PMNode): Map<number, number> {
  const out = new Map<number, number>();
  doc.descendants((node, pos) => {
    const s = node.attrs?.suggestion as { id?: number } | null;
    if (s?.id !== undefined && !out.has(s.id)) out.set(s.id, pos);
    for (const mark of node.marks) {
      const id = mark.attrs?.id as number | undefined;
      if (
        (mark.type.name === "suggestInsert" ||
          mark.type.name === "suggestDelete") &&
        id !== undefined &&
        !out.has(id)
      )
        out.set(id, pos);
    }
    return true;
  });
  return out;
}
