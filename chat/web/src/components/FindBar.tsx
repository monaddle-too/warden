import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type RefObject,
} from "react";
import { isKey, modifierKey } from "../shortcuts";
import { ChevronDown, ChevronUp, Search, X } from "lucide-react";
import { entryHits, findMatches, locate } from "../search";
import type { Entry } from "../types";

/* What opens the find bar: a query (empty for a fresh ⌘F) and, from the
   palette, the entry to land in. Requests are told apart by identity, so
   a new object asks again even with the same query. */
export type FindRequest = { chatID: string; query: string; entryID?: string };

/* Whether ⌘F / Ctrl+F was pressed, on either platform's modifier
   (shortcuts.ts). */
export const isFindKey = (event: KeyboardEvent) => isKey(event, "find");

export { modifierKey };

// Matches are painted with the CSS Custom Highlight API (`::highlight()` in
// conversation.css), which styles ranges of the rendered text without
// touching the DOM React owns, so a highlighted block keeps its state and a
// re-render never fights the highlight. Browsers without it still get the
// count and the scrolling.
const ALL = "warden-find";
const CURRENT = "warden-find-current";
const registry = () =>
  typeof CSS !== "undefined" && "highlights" in CSS ? CSS.highlights : null;

const SKIPPED = new Set(["BUTTON", "SCRIPT", "STYLE", "TEXTAREA", "SELECT"]);

/* Text nodes a reader can see under `root`: not in a button (the code
   block's header, the message actions), not inside a closed <details>
   (its summary is fine), and not KaTeX's clipped MathML copy of a formula,
   which would count every formula twice. */
function visibleText(root: Element): Text[] {
  const out: Text[] = [];
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
    acceptNode(node) {
      for (
        let el = node.parentElement;
        el && el !== root;
        el = el.parentElement
      ) {
        if (
          SKIPPED.has(el.tagName) ||
          el.hidden ||
          el.classList.contains("katex-mathml")
        )
          return NodeFilter.FILTER_REJECT;
        const parent = el.parentElement;
        if (
          parent instanceof HTMLDetailsElement &&
          !parent.open &&
          el.tagName !== "SUMMARY"
        )
          return NodeFilter.FILTER_REJECT;
      }
      return NodeFilter.FILTER_ACCEPT;
    },
  });
  for (let n = walker.nextNode(); n; n = walker.nextNode()) out.push(n as Text);
  return out;
}

/* One Range per match of `query` in the rendered text under `root`, found
   per block (a child of the transcript) so a match may span inline
   elements (**bold** text) but never two entries. */
export function matchRanges(root: Element, query: string): Range[] {
  const ranges: Range[] = [];
  if (!query.trim()) return ranges;
  for (const block of root.children) {
    const nodes = visibleText(block);
    if (!nodes.length) continue;
    const lengths = nodes.map((n) => n.data.length);
    const text = nodes.map((n) => n.data).join("");
    for (const match of findMatches(text, query)) {
      const start = locate(lengths, match.start);
      const end = locate(lengths, match.end, true);
      const range = document.createRange();
      range.setStart(nodes[start.index], start.offset);
      range.setEnd(nodes[end.index], end.offset);
      ranges.push(range);
    }
  }
  return ranges;
}

/* Opens every <details> from `el` up to the transcript root: the entry's
   own card, the step group it sits in, a subagent's transcript and the
   Agent card around it. True when one was closed. */
function openAncestors(el: Element): boolean {
  let opened = false;
  for (
    let details: Element | null = el;
    details;
    details = details.parentElement?.closest("details") ?? null
  )
    if (details instanceof HTMLDetailsElement && !details.open) {
      details.open = true;
      opened = true;
    }
  return opened;
}

/* The transcript element for an entry, opened if it sits in a collapsed
   activity group and scrolled into view. */
function reveal(root: Element, entryID: string): Element | null {
  const el = root.querySelector(`[data-entry="${CSS.escape(entryID)}"]`);
  if (!el) return null;
  openAncestors(el);
  el.scrollIntoView({ block: "center" });
  return el;
}

/* Brings the matches the rendered text hides into view: an entry the
   service says matches (search.ts `entryHits`) but whose visible text
   does not is inside a closed card (a subagent's step under its Agent
   card, a step in a collapsed group) or past a card's "+N lines" fold, so
   the cards are opened and the entry's own fold unfolded, as the reader
   would. Each entry is handled once per query (`done`); one not rendered
   yet is tried again next time. True when something changed, which the
   observer follows with another pass. */
function revealHits(
  root: Element,
  query: string,
  entries: Entry[],
  done: Set<string>,
): boolean {
  let changed = false;
  for (const { id, field } of entryHits(entries, query)) {
    if (done.has(id)) continue;
    const el = root.querySelector(`[data-entry="${CSS.escape(id)}"]`);
    if (!el) continue;
    done.add(id);
    if (openAncestors(el)) changed = true;
    if (field !== "detail") continue;
    const own = visibleText(el)
      .map((n) => n.data)
      .join("");
    if (findMatches(own, query, 1).length) continue;
    // The entry's own output fold, not a nested entry's and not the
    // prompt or input a card also folds.
    for (const button of el.querySelectorAll<HTMLButtonElement>(
      'button.tool-more[aria-expanded="false"]',
    ))
      if (
        button.closest("[data-entry]") === el &&
        !button.closest(".tool-prompt, .tool-input")
      ) {
        button.click();
        changed = true;
      }
  }
  return changed;
}

/* Matches of `query` in the rendered transcript, kept current as it
   changes (a streamed chunk, a diagram replacing its source, a group
   opened) and painted as highlights; `current` is the one scrolled to. A
   new query starts at its first match; a new request with an entry starts
   at the first match inside that entry. What the transcript hides (a
   subagent's steps, a folded output) is opened first, from `entries`. */
function useFind(
  root: RefObject<HTMLElement | null>,
  scroller: RefObject<HTMLElement | null>,
  query: string,
  request: FindRequest,
  entries: Entry[],
) {
  const [found, setFound] = useState<{ ranges: Range[]; current: number }>({
    ranges: [],
    current: 0,
  });
  const latest = useRef(found);
  latest.current = found;
  // The entries as they are now, for a pass the observer starts; the
  // effect itself must not rerun on every streamed chunk.
  const known = useRef(entries);
  known.current = entries;
  const scrollPending = useRef(false);
  const landed = useRef<FindRequest>(undefined);
  useEffect(() => {
    const el = root.current;
    if (!el) return;
    let landing: Element | null = null;
    if (landed.current !== request) {
      landed.current = request;
      landing = request.entryID ? reveal(el, request.entryID) : null;
    }
    let fresh = true;
    let frame = 0;
    const revealed = new Set<string>();
    const compute = () => {
      frame = 0;
      const opened = revealHits(el, query, known.current, revealed);
      const ranges = matchRanges(el, query);
      let current = Math.min(
        latest.current.current,
        Math.max(0, ranges.length - 1),
      );
      if (fresh) {
        const inside = landing
          ? ranges.findIndex((r) => landing!.contains(r.startContainer))
          : -1;
        if (landing && inside < 0 && opened) {
          // The landing's match is in what was just opened; the pass the
          // change triggers will find it.
          setFound({ ranges, current });
          return;
        }
        fresh = false;
        current = Math.max(0, inside);
        scrollPending.current = landing ? inside >= 0 : ranges.length > 0;
        landing = null;
      }
      setFound({ ranges, current });
    };
    compute();
    // A rendered transcript keeps changing under a search; the ranges are
    // rebuilt at most once a frame.
    const observer = new MutationObserver(() => {
      if (!frame) frame = requestAnimationFrame(compute);
    });
    observer.observe(el, {
      subtree: true,
      childList: true,
      characterData: true,
      attributes: true,
      attributeFilter: ["open", "hidden"],
    });
    return () => {
      observer.disconnect();
      if (frame) cancelAnimationFrame(frame);
    };
  }, [root, query, request]);
  const { ranges, current } = found;
  useLayoutEffect(() => {
    const highlights = registry();
    if (!highlights) return;
    if (!ranges.length) {
      highlights.delete(ALL);
      highlights.delete(CURRENT);
      return;
    }
    highlights.set(ALL, new Highlight(...ranges));
    highlights.set(CURRENT, new Highlight(ranges[current]));
    return () => {
      highlights.delete(ALL);
      highlights.delete(CURRENT);
    };
  }, [ranges, current]);
  useEffect(() => {
    if (!scrollPending.current) return;
    scrollPending.current = false;
    const range = ranges[current];
    const box = scroller.current;
    if (!range || !box) return;
    const r = range.getBoundingClientRect();
    const b = box.getBoundingClientRect();
    if (r.top < b.top + 48 || r.bottom > b.bottom - 48)
      box.scrollTop += r.top + r.height / 2 - (b.top + b.height / 2);
  }, [ranges, current, scroller]);
  const step = (by: number) => {
    if (!latest.current.ranges.length) return;
    scrollPending.current = true;
    setFound((f) => ({
      ...f,
      current: (f.current + by + f.ranges.length) % f.ranges.length,
    }));
  };
  return {
    count: ranges.length,
    current,
    next: () => step(1),
    prev: () => step(-1),
  };
}

/* The bar above the transcript: query, "n of m", previous, next, close.
   Enter and Shift+Enter step through the matches, Escape closes. */
export function FindBar({
  root,
  scroller,
  request,
  entries,
  onClose,
}: {
  root: RefObject<HTMLElement | null>;
  scroller: RefObject<HTMLElement | null>;
  request: FindRequest;
  /* The chat's entries, a subagent's nested ones included: what a match
     may hide in. */
  entries: Entry[];
  onClose: () => void;
}) {
  const [query, setQuery] = useState(request.query);
  // A new request replaces the query in the same render, so the matches
  // are never computed for the old one first.
  const [seen, setSeen] = useState(request);
  if (seen !== request) {
    setSeen(request);
    setQuery(request.query);
  }
  const input = useRef<HTMLInputElement>(null);
  // After the palette's dialog has closed and put focus back where it was.
  useEffect(() => {
    const frame = requestAnimationFrame(() => {
      input.current?.focus();
      input.current?.select();
    });
    return () => cancelAnimationFrame(frame);
  }, [request]);
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (!isFindKey(event)) return;
      event.preventDefault();
      input.current?.focus();
      input.current?.select();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);
  const { count, current, next, prev } = useFind(
    root,
    scroller,
    query,
    request,
    entries,
  );
  const asked = query.trim() !== "";
  return (
    <div className="find-bar" role="search" aria-label="Find in chat">
      <Search size={15} aria-hidden="true" />
      <input
        ref={input}
        type="text"
        aria-label="Find in chat"
        placeholder="Find in chat…"
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        onKeyDown={(e) => {
          if (isKey(e, "find-prev")) {
            e.preventDefault();
            prev();
          } else if (isKey(e, "find-next")) {
            e.preventDefault();
            next();
          } else if (isKey(e, "find-close")) {
            e.preventDefault();
            onClose();
          }
        }}
      />
      <span
        className={`find-count${asked && !count ? " none" : ""}`}
        aria-live="polite"
      >
        {asked ? (count ? `${current + 1} of ${count}` : "No matches") : ""}
      </span>
      <button
        type="button"
        className="ghost icon"
        aria-label="Previous match"
        title="Previous match (Shift+Enter)"
        disabled={!count}
        onClick={prev}
      >
        <ChevronUp size={16} />
      </button>
      <button
        type="button"
        className="ghost icon"
        aria-label="Next match"
        title="Next match (Enter)"
        disabled={!count}
        onClick={next}
      >
        <ChevronDown size={16} />
      </button>
      <button
        type="button"
        className="ghost icon"
        aria-label="Close find"
        title="Close (Escape)"
        onClick={onClose}
      >
        <X size={16} />
      </button>
    </div>
  );
}
