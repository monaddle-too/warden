/* The suggestion document: an ordinary Tiptap document in which changed text
   carries suggestInsert / suggestDelete marks and changed blocks a
   `suggestion` attribute, with deleted text kept in place the way Google
   Docs keeps it. The node set is exactly what a reviewed Google Doc edit can
   express (headings, paragraphs, nested lists, bold, italic, links, soft
   breaks) plus read-only atoms for everything else; the server produces and
   consumes this shape (Warden: policy/docview.go). */
import { Extension, Mark, Node, mergeAttributes } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";
import { readOnlySuggestions } from "./plugin";

export type SuggestionKind =
  "replace" | "insert" | "delete" | "restyle" | "accepted";
export type SuggestionAttr = {
  id: number;
  kind: SuggestionKind;
  from?: string;
};
export type SuggestionCard = {
  id: number;
  kind: SuggestionKind;
  summary: string;
  reasons: string[];
  status: "pending" | "accepted" | "rejected";
  acceptable?: boolean;
  hunk?: number;
};
export type DocComment = {
  paragraph: number;
  from?: number;
  to?: number;
  quote?: string;
  text: string;
};

const idAttribute = {
  id: {
    default: 0,
    parseHTML: (element: HTMLElement) =>
      Number(element.getAttribute("data-suggestion-id") || 0),
    renderHTML: (attributes: { id: number }) => ({
      "data-suggestion-id": String(attributes.id),
    }),
  },
};

/* Inserted text. Not inclusive: typing at its edge is the owner's own text. */
export const SuggestInsert = Mark.create({
  name: "suggestInsert",
  inclusive: false,
  addAttributes: () => idAttribute,
  parseHTML: () => [{ tag: "span[data-suggest-insert]" }],
  renderHTML: ({ HTMLAttributes }) => [
    "span",
    mergeAttributes(HTMLAttributes, {
      "data-suggest-insert": "",
      class: "suggest-insert",
    }),
    0,
  ],
});

/* Deleted text: shown struck through, never editable, dropped on save. */
export const SuggestDelete = Mark.create({
  name: "suggestDelete",
  inclusive: false,
  addAttributes: () => idAttribute,
  parseHTML: () => [{ tag: "span[data-suggest-delete]" }],
  renderHTML: ({ HTMLAttributes }) => [
    "span",
    mergeAttributes(HTMLAttributes, {
      "data-suggest-delete": "",
      class: "suggest-delete",
    }),
    0,
  ],
});

/* Block-level facts the server sets: which suggestion a whole paragraph
   belongs to, the Google named style a heading node cannot express, and
   list wrappers the model has no paragraph for. */
export const SuggestionAttributes = Extension.create({
  name: "suggestionAttributes",
  addGlobalAttributes() {
    return [
      {
        types: ["paragraph", "heading"],
        attributes: {
          suggestion: {
            default: null,
            parseHTML: (element) => {
              const id = element.getAttribute("data-suggestion-id");
              return id
                ? {
                    id: Number(id),
                    kind: element.getAttribute("data-suggestion-kind"),
                    from:
                      element.getAttribute("data-suggestion-from") || undefined,
                  }
                : null;
            },
            renderHTML: (attributes) => {
              const s = attributes.suggestion as SuggestionAttr | null;
              if (!s) return {};
              return {
                "data-suggestion-id": String(s.id),
                "data-suggestion-kind": s.kind,
                ...(s.from ? { "data-suggestion-from": s.from } : {}),
                class: `suggest-block suggest-block-${s.kind}`,
              };
            },
          },
          docStyle: {
            default: null,
            parseHTML: (element) => element.getAttribute("data-doc-style"),
            renderHTML: (attributes) =>
              attributes.docStyle
                ? {
                    "data-doc-style": attributes.docStyle,
                    class: `doc-style-${attributes.docStyle}`,
                  }
                : {},
          },
          synthetic: {
            default: null,
            parseHTML: (element) =>
              element.hasAttribute("data-synthetic") ? true : null,
            renderHTML: (attributes) =>
              attributes.synthetic ? { "data-synthetic": "" } : {},
          },
        },
      },
    ];
  },
});

/* Content the model cannot edit (images, footnotes, breaks, chips…): an
   atom the page shows and the keyboard cannot remove. */
export const FrozenBlock = Node.create({
  name: "frozenBlock",
  group: "block",
  atom: true,
  selectable: false,
  draggable: false,
  addAttributes: () => ({
    key: { default: 0 },
    kind: { default: "" },
    label: { default: "" },
  }),
  parseHTML: () => [{ tag: "div[data-frozen-block]" }],
  renderHTML: ({ node }) => [
    "div",
    {
      "data-frozen-block": "",
      "data-frozen-key": String(node.attrs.key),
      class: `frozen-block frozen-${String(node.attrs.kind).replace(/\s+/g, "-")}`,
      contenteditable: "false",
      title: "This content cannot be changed through suggestions",
    },
    String(node.attrs.label),
  ],
});

/* A table, read-only, with its cell text. */
export const FrozenTable = Node.create({
  name: "frozenTable",
  group: "block",
  atom: true,
  selectable: false,
  draggable: false,
  addAttributes: () => ({
    key: { default: 0 },
    label: { default: "" },
    rows: { default: [] as string[][] },
  }),
  parseHTML: () => [{ tag: "table[data-frozen-table]" }],
  renderHTML: ({ node }) => {
    const rows = (node.attrs.rows as string[][]) ?? [];
    return [
      "table",
      {
        "data-frozen-table": "",
        "data-frozen-key": String(node.attrs.key),
        class: "frozen-table",
        contenteditable: "false",
        title: "Tables cannot be changed through suggestions",
      },
      [
        "tbody",
        ...rows.map((row) => [
          "tr",
          ...row.map((cell) => [
            "td",
            {},
            ...cell
              .split("\n")
              .flatMap((line, i) => (i ? [["br"], line] : [line])),
          ]),
        ]),
      ],
    ] as never;
  },
});

/* Everything the review page needs. */
export function suggestionExtensions() {
  return [
    StarterKit.configure({
      blockquote: false,
      code: false,
      codeBlock: false,
      horizontalRule: false,
      strike: false,
      underline: false,
      trailingNode: false,
      heading: { levels: [1, 2, 3, 4, 5, 6] },
      link: { openOnClick: false, autolink: false, linkOnPaste: true },
    }),
    SuggestionAttributes,
    SuggestInsert,
    SuggestDelete,
    FrozenBlock,
    FrozenTable,
    readOnlySuggestions(),
  ];
}
