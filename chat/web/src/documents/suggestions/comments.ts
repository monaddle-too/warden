/* Comment highlights on the page. Comments are anchored to draft paragraph
   numbers and text offsets (see positions.ts), not to editor positions, so
   they survive the page being replaced by a fresh server view. */
import { Extension } from "@tiptap/core";
import { Plugin, PluginKey } from "@tiptap/pm/state";
import { Decoration, DecorationSet } from "@tiptap/pm/view";
import { commentRange } from "./positions";
import type { DocComment } from "./schema";

export const commentHighlightsKey = new PluginKey("commentHighlights");

export function commentHighlights(
  comments: () => DocComment[],
  onSelect: (index: number) => void,
) {
  return Extension.create({
    name: "commentHighlights",
    addProseMirrorPlugins() {
      return [
        new Plugin({
          key: commentHighlightsKey,
          props: {
            decorations(state) {
              const decorations: Decoration[] = [];
              comments().forEach((comment, index) => {
                const range = commentRange(state.doc, comment);
                if (!range) return;
                const attrs = {
                  class: "comment-highlight",
                  "data-comment-index": String(index),
                };
                decorations.push(
                  range.to - range.from > 0 &&
                    state.doc.resolve(range.from).parent.isTextblock
                    ? Decoration.inline(range.from, range.to, attrs)
                    : Decoration.node(range.from, range.to, attrs),
                );
              });
              return DecorationSet.create(state.doc, decorations);
            },
            handleClick(_view, _pos, event) {
              const index = (event.target as HTMLElement)
                .closest("[data-comment-index]")
                ?.getAttribute("data-comment-index");
              if (index !== undefined && index !== null)
                onSelect(Number(index));
              return false;
            },
          },
        }),
      ];
    },
  });
}
