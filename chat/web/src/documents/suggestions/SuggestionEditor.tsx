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
import { TextSelection } from "@tiptap/pm/state";
import type { Editor, JSONContent } from "@tiptap/core";
import {
  suggestionExtensions,
  type DocComment,
  type SuggestionCard,
} from "./schema";
import { commentHighlights } from "./comments";
import { activeHighlight, type ActiveKey } from "./active";
import {
  anchorPosition,
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
  /** A decision is in flight: the cards wait, the page stays editable. */
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
  const paper = useRef<HTMLDivElement>(null);
  const commentsRef = useRef(comments);
  commentsRef.current = comments;
  const [selected, setSelected] = useState<ActiveKey>();
  const selectedRef = useRef<ActiveKey>(undefined);
  selectedRef.current = selected;
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
      commentHighlights(() => commentsRef.current),
      activeHighlight(
        () => selectedRef.current,
        () => commentsRef.current,
        (key) => setSelected(key),
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
  // A fresh server view changes only what differs from the page: one
  // transaction over the changed range, so the caret maps through it and
  // the rest of the page is not rebuilt. (React may run this against an
  // editor a remount has already destroyed.)
  useEffect(() => {
    if (!editor || editor.isDestroyed) return;
    try {
      const next = editor.schema.nodeFromJSON(doc);
      next.check();
      const current = editor.state.doc;
      const start = current.content.findDiffStart(next.content);
      if (start !== null) {
        // The caret's home is a draft paragraph and an offset into its
        // surviving text, which the new page still has; ProseMirror's
        // own mapping would push a caret at the edge of the replaced
        // range past the paragraph.
        const { from, empty } = editor.state.selection;
        const home = empty ? selectionAnchor(current, from, from) : null;
        const end = current.content.findDiffEnd(next.content)!;
        const overlap = start - Math.min(end.a, end.b);
        const oldEnd = overlap > 0 ? end.a + overlap : end.a;
        const newEnd = overlap > 0 ? end.b + overlap : end.b;
        const tr = editor.state.tr
          .replace(start, oldEnd, next.slice(start, newEnd))
          .setMeta("suggestions", "server")
          .setMeta("addToHistory", false);
        if (home) {
          const pos = anchorPosition(tr.doc, home.paragraph, home.from ?? 0);
          if (pos !== null)
            tr.setSelection(TextSelection.near(tr.doc.resolve(pos)));
        }
        editor.view.dispatch(tr);
      }
    } catch {
      editor
        .chain()
        .setMeta("suggestions", "server")
        .setContent(doc, { emitUpdate: false })
        .run();
    }
    pending.current = {};
    setComposer(undefined);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [revision, editor]);
  // Saving must not take the page away from under the caret: only the
  // review's state decides whether the page is editable.
  useEffect(() => {
    if (editor && !editor.isDestroyed) editor.setEditable(editable, false);
  }, [editor, editable]);
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
      const { from, empty } = editor.state.selection;
      if (empty || !paper.current || !editable) {
        setSelection(undefined);
        return;
      }
      try {
        // The button sits inside the paper, so offsets are the paper's.
        const rect = paper.current.getBoundingClientRect();
        const start = editor.view.coordsAtPos(from);
        setSelection({
          top: start.top - rect.top - 34,
          left: start.left - rect.left,
        });
      } catch {
        setSelection(undefined);
      }
    };
    editor.on("selectionUpdate", onSelection);
    return () => {
      editor.off("selectionUpdate", onSelection);
    };
  }, [editor, editable]);
  // The selection lives outside the document: poke the view so the
  // active decoration recomputes, and bring the selected card into view.
  useEffect(() => {
    if (editor && !editor.isDestroyed)
      editor.view.dispatch(editor.state.tr.setMeta("active", selected));
    if (!selected || selected === "composer") return;
    const card = container.current?.querySelector<HTMLElement>(
      `[data-margin-key="${selected.replace(/["\\]/g, "")}"]`,
    );
    card?.scrollIntoView?.({ block: "nearest", behavior: "smooth" });
  }, [editor, selected, revision]);

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
            >
              <b>B</b>
            </button>
            <button
              type="button"
              aria-pressed={editor.isActive("italic")}
              onClick={() => editor.chain().focus().toggleItalic().run()}
            >
              <i>I</i>
            </button>
            <button
              type="button"
              aria-pressed={editor.isActive("link")}
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
            >
              • List
            </button>
            <button
              type="button"
              aria-pressed={editor.isActive("orderedList")}
              onClick={() => editor.chain().focus().toggleOrderedList().run()}
            >
              1. List
            </button>
            <button
              type="button"
              onClick={() =>
                editor.chain().focus().sinkListItem("listItem").run()
              }
              disabled={!editor.can().sinkListItem("listItem")}
              title="Indent"
            >
              →
            </button>
            <button
              type="button"
              onClick={() =>
                editor.chain().focus().liftListItem("listItem").run()
              }
              disabled={!editor.can().liftListItem("listItem")}
              title="Outdent"
            >
              ←
            </button>
            <button
              type="button"
              onClick={() => editor.chain().focus().undo().run()}
              disabled={!editor.can().undo()}
              title="Undo"
            >
              ↶
            </button>
            <button
              type="button"
              onClick={() => editor.chain().focus().redo().run()}
              disabled={!editor.can().redo()}
              title="Redo"
            >
              ↷
            </button>
          </div>
        )}
        <div className="suggestion-paper" ref={paper}>
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
