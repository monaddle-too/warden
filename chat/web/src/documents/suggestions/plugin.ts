/* Editing rules of the review page: deleted text and frozen content are
   read-only; a whole paragraph the suggestion deletes cannot be typed into.
   Accepting and rejecting are not done by editing the page but by asking
   the server, which recomputes the page; so any transaction that would
   change those ranges is refused outright. */
import { Extension } from "@tiptap/core";
import { Plugin, PluginKey, type Transaction } from "@tiptap/pm/state";
import type { Node as PMNode } from "@tiptap/pm/model";
import type { Step } from "@tiptap/pm/transform";

export const readOnlySuggestionsKey = new PluginKey("readOnlySuggestions");

type Ranged = Step & { from: number; to: number };

/* True when [from, to] of doc touches deleted text or frozen content. */
export function touchesProtected(doc: PMNode, from: number, to: number) {
  const deleteMark = doc.type.schema.marks.suggestDelete;
  if (from < to) {
    if (deleteMark && doc.rangeHasMark(from, to, deleteMark)) return true;
    let frozen = false;
    doc.nodesBetween(from, to, (node) => {
      if (node.type.name === "frozenBlock" || node.type.name === "frozenTable")
        frozen = true;
      return !frozen;
    });
    return frozen;
  }
  const $pos = doc.resolve(from);
  if (deleteMark && $pos.marks().some((m) => m.type === deleteMark))
    return true;
  const parent = $pos.parent;
  const suggestion = parent.attrs?.suggestion as { kind?: string } | null;
  return !!suggestion && suggestion.kind === "delete";
}

export function readOnlySuggestions() {
  return Extension.create({
    name: "readOnlySuggestions",
    addProseMirrorPlugins() {
      return [
        new Plugin({
          key: readOnlySuggestionsKey,
          filterTransaction(tr: Transaction) {
            if (!tr.docChanged || tr.getMeta("suggestions") === "server")
              return true;
            return tr.steps.every((step, i) => {
              const ranged = step as Ranged;
              if (
                typeof ranged.from !== "number" ||
                typeof ranged.to !== "number"
              )
                return true;
              return !touchesProtected(tr.docs[i], ranged.from, ranged.to);
            });
          },
        }),
      ];
    },
  });
}
