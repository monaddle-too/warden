/* The right margin: one card per suggestion and per comment, each placed
   beside the first line it refers to, pushed down where they would
   overlap — the way Google Docs lays out its suggestion and comment cards. */
import { useLayoutEffect, useState, type ReactNode } from "react";
import type { DocComment, SuggestionCard } from "./schema";

export type MarginItem =
  | { kind: "suggestion"; key: string; top: number; card: SuggestionCard }
  | {
      kind: "comment";
      key: string;
      top: number;
      index: number;
      comment: DocComment;
    }
  | { kind: "composer"; key: string; top: number };

const CARD_GAP = 8;

/* Stacks items so none overlaps the one above it; items without a place
   on the page (rejected suggestions) follow the last placed one. */
export function layout(items: MarginItem[], heights: Record<string, number>) {
  const placed = items
    .filter((item) => Number.isFinite(item.top))
    .sort((a, b) => a.top - b.top);
  const unplaced = items.filter((item) => !Number.isFinite(item.top));
  let floor = 0;
  return [...placed, ...unplaced].map((item) => {
    const top = Math.max(Number.isFinite(item.top) ? item.top : 0, floor);
    floor = top + (heights[item.key] ?? 96) + CARD_GAP;
    return { ...item, top };
  });
}

const KIND_LABEL: Record<string, string> = {
  replace: "Replace",
  insert: "Add",
  delete: "Delete",
  restyle: "Format",
};

export function Margin({
  items,
  author,
  selected,
  disabled,
  onSelect,
  onAccept,
  onReject,
  onRestore,
  onRemoveComment,
  composer,
}: {
  items: MarginItem[];
  author: string;
  selected?: string;
  disabled?: boolean;
  onSelect: (key: string) => void;
  onAccept: (changeID: number) => void;
  onReject: (changeID: number) => void;
  onRestore: (hunkID: number) => void;
  onRemoveComment: (index: number) => void;
  composer?: ReactNode;
}) {
  const [heights, setHeights] = useState<Record<string, number>>({});
  const placed = layout(items, heights);
  // Measure after each render so stacking uses real card heights.
  useLayoutEffect(() => {
    const next: Record<string, number> = {};
    let changed = false;
    for (const item of items) {
      const el = document.querySelector<HTMLElement>(
        `[data-margin-key="${item.key.replace(/["\\]/g, "")}"]`,
      );
      if (!el) continue;
      next[item.key] = el.offsetHeight;
      if (heights[item.key] !== el.offsetHeight) changed = true;
    }
    if (changed) setHeights((old) => ({ ...old, ...next }));
  });
  const height = placed.reduce(
    (n, item) => Math.max(n, item.top + (heights[item.key] ?? 96) + CARD_GAP),
    0,
  );
  return (
    <aside className="suggestion-margin" style={{ minHeight: height }}>
      {placed.map((item) => {
        const active = selected === item.key;
        const style = { top: item.top };
        if (item.kind === "composer")
          return (
            <div
              key={item.key}
              data-margin-key={item.key}
              className="suggestion-card suggestion-card-composer active"
              style={style}
            >
              {composer}
            </div>
          );
        if (item.kind === "comment")
          return (
            <div
              key={item.key}
              data-margin-key={item.key}
              className={`suggestion-card suggestion-card-comment ${active ? "active" : ""}`}
              style={style}
              onClick={() => onSelect(item.key)}
            >
              <header>
                <strong>You</strong>
                <span className="muted">comment</span>
              </header>
              {item.comment.quote && (
                <blockquote>{item.comment.quote}</blockquote>
              )}
              <p>{item.comment.text}</p>
              {!disabled && (
                <footer>
                  <button
                    type="button"
                    onClick={(e) => {
                      e.stopPropagation();
                      onRemoveComment(item.index);
                    }}
                  >
                    Remove
                  </button>
                </footer>
              )}
            </div>
          );
        const { card } = item;
        return (
          <div
            key={item.key}
            data-margin-key={item.key}
            className={`suggestion-card suggestion-card-${card.status} ${active ? "active" : ""}`}
            style={style}
            onClick={() => onSelect(item.key)}
          >
            <header>
              <strong>{author}</strong>
              <span className="muted">
                {card.status === "pending"
                  ? (KIND_LABEL[card.kind] ?? card.kind)
                  : card.status}
              </span>
            </header>
            <p className="suggestion-summary">{card.summary}</p>
            {card.reasons.map((reason, i) => (
              <p key={i} className="suggestion-reason">
                {reason}
              </p>
            ))}
            {!disabled && (
              <footer>
                {card.status === "pending" && (
                  <>
                    <button
                      type="button"
                      className="suggestion-accept"
                      aria-label="Accept suggestion"
                      title="Accept"
                      onClick={(e) => {
                        e.stopPropagation();
                        onAccept(card.id);
                      }}
                    >
                      ✓
                    </button>
                    <button
                      type="button"
                      className="suggestion-reject"
                      aria-label="Reject suggestion"
                      title="Reject"
                      onClick={(e) => {
                        e.stopPropagation();
                        onReject(card.id);
                      }}
                    >
                      ✕
                    </button>
                  </>
                )}
                {card.status === "accepted" && (
                  <button
                    type="button"
                    className="suggestion-reject"
                    aria-label="Reject suggestion"
                    title="Reject after all"
                    onClick={(e) => {
                      e.stopPropagation();
                      onReject(card.id);
                    }}
                  >
                    ✕
                  </button>
                )}
                {card.status === "rejected" && card.hunk !== undefined && (
                  <button
                    type="button"
                    disabled={card.acceptable === false}
                    title={
                      card.acceptable === false
                        ? "Those paragraphs were edited by hand"
                        : "Put the suggestion back"
                    }
                    onClick={(e) => {
                      e.stopPropagation();
                      onRestore(card.hunk!);
                    }}
                  >
                    Restore
                  </button>
                )}
              </footer>
            )}
          </div>
        );
      })}
    </aside>
  );
}
