/* The link between the margin and the page: whichever card or range is
   selected, its counterpart lights up. The selection is held outside the
   document (a ref the plugins read), the highlight is a decoration so it
   survives ProseMirror redrawing the page, and a click on a suggestion's
   text selects it. */
import { Extension } from "@tiptap/core";
import { Plugin, PluginKey } from "@tiptap/pm/state";
import { Decoration, DecorationSet } from "@tiptap/pm/view";
import { commentRange } from "./positions";
import type { DocComment } from "./schema";

export const activeKey = new PluginKey("activeSuggestion");

/* "suggestion:<id>" or "comment:<index>" or undefined. */
export type ActiveKey = string | undefined;

export function activeHighlight(
  active: () => ActiveKey,
  comments: () => DocComment[],
  onSelect: (key: ActiveKey) => void,
) {
  return Extension.create({
    name: "activeSuggestion",
    addProseMirrorPlugins() {
      return [
        new Plugin({
          key: activeKey,
          props: {
            decorations(state) {
              const key = active();
              if (!key) return DecorationSet.empty;
              const [kind, raw] = key.split(":");
              const decorations: Decoration[] = [];
              if (kind === "suggestion") {
                const id = Number(raw);
                state.doc.descendants((node, pos) => {
                  const s = node.attrs?.suggestion as { id?: number } | null;
                  if (s?.id === id)
                    decorations.push(
                      Decoration.node(pos, pos + node.nodeSize, {
                        class: "suggest-active",
                      }),
                    );
                  if (
                    node.isText &&
                    node.marks.some(
                      (m) =>
                        (m.type.name === "suggestInsert" ||
                          m.type.name === "suggestDelete") &&
                        m.attrs.id === id,
                    )
                  )
                    decorations.push(
                      Decoration.inline(pos, pos + node.nodeSize, {
                        class: "suggest-active",
                      }),
                    );
                  return true;
                });
              } else if (kind === "comment") {
                const comment = comments()[Number(raw)];
                const range = comment && commentRange(state.doc, comment);
                if (range)
                  decorations.push(
                    range.to - range.from > 0 &&
                      state.doc.resolve(range.from).parent.isTextblock
                      ? Decoration.inline(range.from, range.to, {
                          class: "suggest-active",
                        })
                      : Decoration.node(range.from, range.to, {
                          class: "suggest-active",
                        }),
                  );
              }
              return DecorationSet.create(state.doc, decorations);
            },
            handleClick(_view, _pos, event) {
              const target = event.target as HTMLElement;
              const comment = target
                .closest("[data-comment-index]")
                ?.getAttribute("data-comment-index");
              if (comment !== undefined && comment !== null) {
                onSelect("comment:" + comment);
                return false;
              }
              const id = target
                .closest("[data-suggestion-id]")
                ?.getAttribute("data-suggestion-id");
              onSelect(id ? "suggestion:" + id : undefined);
              return false;
            },
          },
        }),
      ];
    },
  });
}
