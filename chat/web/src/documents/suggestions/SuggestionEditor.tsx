/* The review page: the document with suggestions inline, a margin of
   cards, and the owner editing the draft directly. Decisions and edits go
   to the caller (the server keeps the draft and recomputes the page); a
   new `revision` replaces the page with the server's view. */
import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { EditorContent, useEditor, useEditorState } from "@tiptap/react";
import type { Editor, JSONContent } from "@tiptap/core";
import {
  suggestionExtensions,
  type DocComment,
  type SuggestionCard,
} from "./schema";
import { commentHighlights } from "./comments";
import {
  commentRange,
  selectionAnchor,
  suggestionPositions,
} from "./positions";
import { Margin, type MarginItem } from "./Margin";
import "../rich-documents.css";
import "./suggestions.css";

export interface SuggestionEditorProps {
  document: JSONContent;
  /** Bump when `document` is a fresh server view; the page is replaced. */
  revision: number;
  suggestions: SuggestionCard[];
  comments: DocComment[];
  editable: boolean;
  /** Who made the suggestions, for the cards ("Claude", "Codex"). */
  author: string;
  busy?: boolean;
  /** The edited page, debounced; deleted text and marks are the server's to strip. */
  onSave: (document: JSONContent) => void;
  onAccept: (changeID: number) => void;
  onReject: (changeID: number) => void;
  onRestore: (hunkID: number) => void;
  onComments: (comments: DocComment[]) => void;
  /** Rendered above the page (a status line, say). */
  header?: ReactNode;
}

const SAVE_DELAY = 600;
const STYLES: [string, string][] = [
  ["text", "Normal text"],
  ["title", "Title"],
  ["subtitle", "Subtitle"],
  ["h1", "Heading 1"],
  ["h2", "Heading 2"],
  ["h3", "Heading 3"],
  ["h4", "Heading 4"],
];

function currentStyle(editor: Editor) {
  for (let level = 1; level <= 6; level++)
    if (editor.isActive("heading", { level })) return `h${level}`;
  const docStyle = editor.getAttributes("paragraph").docStyle;
  return docStyle === "title" || docStyle === "subtitle" ? docStyle : "text";
}

export function SuggestionEditor({
  document: doc,
  revision,
  suggestions,
  comments,
  editable,
  author,
  busy,
  onSave,
  onAccept,
  onReject,
  onRestore,
  onComments,
  header,
}: SuggestionEditorProps) {
  const container = useRef<HTMLDivElement>(null);
  const commentsRef = useRef(comments);
  commentsRef.current = comments;
  const [selected, setSelected] = useState<string>();
  const [tops, setTops] = useState<Map<string, number>>(new Map());
  const [composer, setComposer] = useState<{
    top: number;
    anchor: Pick<DocComment, "paragraph" | "from" | "to" | "quote">;
  }>();
  const [composerText, setComposerText] = useState("");
  const [selection, setSelection] = useState<{ top: number; left: number }>();
  const pending = useRef<{
    timer?: ReturnType<typeof setTimeout>;
    document?: JSONContent;
  }>({});
  const saveRef = useRef(onSave);
  saveRef.current = onSave;
  const flush = useCallback(() => {
    const p = pending.current;
    if (p.timer) clearTimeout(p.timer);
    const document = p.document;
    pending.current = {};
    if (document) saveRef.current(document);
  }, []);
  const editor = useEditor({
    extensions: [
      ...suggestionExtensions(),
      commentHighlights(
        () => commentsRef.current,
        (index) => setSelected("comment:" + index),
      ),
    ],
    content: doc,
    editable,
    immediatelyRender: true,
    onUpdate: ({ editor, transaction }) => {
      if (transaction.getMeta("suggestions") === "server") return;
      pending.current.document = editor.getJSON();
      if (pending.current.timer) clearTimeout(pending.current.timer);
      pending.current.timer = setTimeout(flush, SAVE_DELAY);
    },
  });
  // A fresh server view replaces the page; keep the caret where it was.
  // (React may run this against an editor a remount has already destroyed.)
  useEffect(() => {
    if (!editor || editor.isDestroyed) return;
    const from = editor.state.selection.from;
    editor
      .chain()
      .setMeta("suggestions", "server")
      .setContent(doc, { emitUpdate: false })
      .run();
    const size = editor.state.doc.content.size;
    editor.commands.setTextSelection(Math.max(0, Math.min(from, size)));
    pending.current = {};
    setComposer(undefined);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [revision, editor]);
  useEffect(() => {
    if (editor && !editor.isDestroyed)
      editor.setEditable(editable && !busy, false); // not an edit, no save
  }, [editor, editable, busy]);
  useEffect(() => () => flush(), [flush]);
  // Comment highlights live outside the document; poke the view.
  useEffect(() => {
    if (editor && !editor.isDestroyed)
      editor.view.dispatch(editor.state.tr.setMeta("comments", true));
  }, [editor, comments]);

  // Where each card goes: the top of the first line of its range.
  const measure = useCallback(() => {
    if (!editor || editor.isDestroyed || !container.current) return;
    const origin = container.current.getBoundingClientRect().top;
    const next = new Map<string, number>();
    const at = (pos: number) => {
      try {
        return editor.view.coordsAtPos(pos).top - origin;
      } catch {
        return 0;
      }
    };
    for (const [id, pos] of suggestionPositions(editor.state.doc))
      next.set(
        "suggestion:" + id,
        at(Math.min(pos + 1, editor.state.doc.content.size)),
      );
    comments.forEach((comment, index) => {
      const range = commentRange(editor.state.doc, comment);
      if (range)
        next.set(
          "comment:" + index,
          at(Math.min(range.from + 1, editor.state.doc.content.size)),
        );
    });
    setTops(next);
  }, [editor, comments]);
  useEffect(() => {
    if (!editor) return;
    measure();
    editor.on("update", measure);
    window.addEventListener("resize", measure);
    return () => {
      editor.off("update", measure);
      window.removeEventListener("resize", measure);
    };
  }, [editor, measure, revision, suggestions]);
  // Selecting text offers a comment.
  useEffect(() => {
    if (!editor) return;
    const onSelection = () => {
      const { from, to, empty } = editor.state.selection;
      if (empty || !container.current || !editable) {
        setSelection(undefined);
        return;
      }
      try {
        const rect = container.current.getBoundingClientRect();
        const start = editor.view.coordsAtPos(from);
        setSelection({
          top: start.top - rect.top - 34,
          left: start.left - rect.left,
        });
      } catch {
        setSelection(undefined);
      }
      void to;
    };
    editor.on("selectionUpdate", onSelection);
    return () => {
      editor.off("selectionUpdate", onSelection);
    };
  }, [editor, editable]);
  // Highlight the page range of the selected card.
  useEffect(() => {
    const root = container.current;
    if (!root) return;
    root
      .querySelectorAll(".suggest-active")
      .forEach((el) => el.classList.remove("suggest-active"));
    if (!selected) return;
    const [kind, id] = selected.split(":");
    const attr =
      kind === "suggestion" ? "data-suggestion-id" : "data-comment-index";
    root
      .querySelectorAll(`[${attr}="${id}"]`)
      .forEach((el) => el.classList.add("suggest-active"));
  }, [selected, revision, tops]);

  const items = useMemo<MarginItem[]>(() => {
    const out: MarginItem[] = [];
    for (const card of suggestions) {
      const key = "suggestion:" + card.id;
      // Rejected suggestions have no range on the page: the margin lists
      // them after the placed cards.
      out.push({ kind: "suggestion", key, card, top: tops.get(key) ?? NaN });
    }

    comments.forEach((comment, index) => {
      const key = "comment:" + index;
      out.push({
        kind: "comment",
        key,
        index,
        comment,
        top: tops.get(key) ?? 0,
      });
    });
    if (composer)
      out.push({ kind: "composer", key: "composer", top: composer.top });
    return out;
  }, [suggestions, comments, tops, composer]);

  const style = useEditorState({
    editor,
    selector: ({ editor }) => (editor ? currentStyle(editor) : "text"),
  });
  const decide = (action: () => void) => {
    flush();
    action();
  };
  const startComment = () => {
    if (!editor || !container.current) return;
    const { from, to } = editor.state.selection;
    const anchor = selectionAnchor(editor.state.doc, from, to);
    if (!anchor) return;
    const origin = container.current.getBoundingClientRect().top;
    setComposer({ top: editor.view.coordsAtPos(from).top - origin, anchor });
    setComposerText("");
    setSelection(undefined);
    setSelected("composer");
  };
  const submitComment = () => {
    if (!composer || !composerText.trim()) return;
    onComments([
      ...comments,
      { ...composer.anchor, text: composerText.trim() },
    ]);
    setComposer(undefined);
    setSelected(undefined);
  };
  if (!editor) return null;
  return (
    <div className="suggestion-review" ref={container}>
      <div className="suggestion-page">
        {header}
        {editable && (
          <div
            className="rich-document-toolbar suggestion-toolbar"
            role="toolbar"
            aria-label="Formatting"
          >
            <select
              aria-label="Paragraph style"
              value={style}
              disabled={busy}
              onChange={(e) => {
                const value = e.target.value;
                const chain = editor.chain().focus();
                if (value.startsWith("h"))
                  chain
                    .setNode("heading", {
                      level: Number(value.slice(1)),
                      docStyle: null,
                    })
                    .run();
                else
                  chain
                    .setNode("paragraph", {
                      docStyle: value === "text" ? null : value,
                    })
                    .run();
              }}
            >
              {STYLES.map(([value, label]) => (
                <option key={value} value={value}>
                  {label}
                </option>
              ))}
            </select>
            <button
              type="button"
              aria-pressed={editor.isActive("bold")}
              onClick={() => editor.chain().focus().toggleBold().run()}
              disabled={busy}
            >
              <b>B</b>
            </button>
            <button
              type="button"
              aria-pressed={editor.isActive("italic")}
              onClick={() => editor.chain().focus().toggleItalic().run()}
              disabled={busy}
            >
              <i>I</i>
            </button>
            <button
              type="button"
              aria-pressed={editor.isActive("link")}
              disabled={busy}
              onClick={() => {
                const current = editor.getAttributes("link").href as
                  string | undefined;
                const href = window.prompt("Link URL", current || "https://");
                if (href === null) return;
                if (!href.trim()) editor.chain().focus().unsetLink().run();
                else
                  editor
                    .chain()
                    .focus()
                    .extendMarkRange("link")
                    .setLink({ href: href.trim() })
                    .run();
              }}
            >
              Link
            </button>
            <button
              type="button"
              aria-pressed={editor.isActive("bulletList")}
              onClick={() => editor.chain().focus().toggleBulletList().run()}
              disabled={busy}
            >
              • List
            </button>
            <button
              type="button"
              aria-pressed={editor.isActive("orderedList")}
              onClick={() => editor.chain().focus().toggleOrderedList().run()}
              disabled={busy}
            >
              1. List
            </button>
            <button
              type="button"
              onClick={() =>
                editor.chain().focus().sinkListItem("listItem").run()
              }
              disabled={busy || !editor.can().sinkListItem("listItem")}
              title="Indent"
            >
              →
            </button>
            <button
              type="button"
              onClick={() =>
                editor.chain().focus().liftListItem("listItem").run()
              }
              disabled={busy || !editor.can().liftListItem("listItem")}
              title="Outdent"
            >
              ←
            </button>
            <button
              type="button"
              onClick={() => editor.chain().focus().undo().run()}
              disabled={busy || !editor.can().undo()}
              title="Undo"
            >
              ↶
            </button>
            <button
              type="button"
              onClick={() => editor.chain().focus().redo().run()}
              disabled={busy || !editor.can().redo()}
              title="Redo"
            >
              ↷
            </button>
          </div>
        )}
        <div className="suggestion-paper">
          <EditorContent
            editor={editor}
            className="rich-document-page suggestion-document"
          />
          {selection && (
            <button
              type="button"
              className="suggestion-comment-button"
              style={{ top: selection.top, left: selection.left }}
              onMouseDown={(e) => e.preventDefault()}
              onClick={startComment}
            >
              💬 Comment
            </button>
          )}
        </div>
      </div>
      <Margin
        items={items}
        author={author}
        selected={selected}
        disabled={!editable || busy}
        onSelect={setSelected}
        onAccept={(id) => decide(() => onAccept(id))}
        onReject={(id) => decide(() => onReject(id))}
        onRestore={(hunk) => decide(() => onRestore(hunk))}
        onRemoveComment={(index) =>
          onComments(comments.filter((_, i) => i !== index))
        }
        composer={
          composer && (
            <>
              <header>
                <strong>You</strong>
                <span className="muted">comment</span>
              </header>
              {composer.anchor.quote && (
                <blockquote>{composer.anchor.quote}</blockquote>
              )}
              <textarea
                aria-label="Comment"
                autoFocus
                value={composerText}
                onChange={(e) => setComposerText(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && (e.metaKey || e.ctrlKey))
                    submitComment();
                  if (e.key === "Escape") setComposer(undefined);
                }}
                placeholder="Note for the agent"
              />
              <footer>
                <button type="button" onClick={() => setComposer(undefined)}>
                  Cancel
                </button>
                <button
                  type="button"
                  className="suggestion-accept"
                  onClick={submitComment}
                  disabled={!composerText.trim()}
                >
                  Comment
                </button>
              </footer>
            </>
          )
        }
      />
    </div>
  );
}
