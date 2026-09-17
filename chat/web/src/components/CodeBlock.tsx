import {
  useCallback,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import {
  Check,
  ChevronsDownUp,
  ChevronsUpDown,
  Code,
  Copy,
  TextWrap,
} from "lucide-react";
import {
  COLLAPSE_LINES,
  codeChild,
  codeLanguage,
  hastText,
  lineCount,
  type HastNode,
} from "../code";
import { fenceDiff, isDiffFence } from "../diff";
import { isMermaidFence } from "../mermaid";
import { DiffView } from "./DiffView";
import { MermaidDiagram, useMermaid } from "./Mermaid";
import { useCopy } from "./useCopy";

/* A fenced block from the transcript: language label, copy, wrap toggle and
   a collapse for long output. `children` is the <code> react-markdown already
   rendered (highlighted spans included); `node` is its hast source, which is
   where the text for the clipboard and the line count come from. A closed
   ```mermaid fence becomes a diagram with a toggle back to its source, and a
   ```diff fence a coloured diff whose hunks fold on their own. */
export function CodeBlock({
  node,
  open,
  children,
}: {
  node?: HastNode;
  /* Set while the fence is still being written (see `touchesEnd`). */
  open?: boolean;
  children?: ReactNode;
}) {
  const code = codeChild(node);
  const language = codeLanguage(code?.properties?.className);
  const text = useMemo(() => hastText(code), [code]);
  const lines = lineCount(text);
  const [wrap, setWrap] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const [source, setSource] = useState(false);
  const { copied, copy } = useCopy(useCallback(() => text, [text]));
  // An open fence may still grow, so the diagram waits for the final text;
  // the source stays visible until it renders.
  const diagram = useMermaid(text, isMermaidFence(language) && !open);
  const svg = source ? undefined : diagram?.svg;
  // The parse is line-by-line string work, safe on a fence that is still
  // streaming; the last hunk simply grows.
  const diff = useMemo(
    () => (isDiffFence(language) ? fenceDiff(text) : undefined),
    [language, text],
  );
  // A fence that is still being written never folds (the reader is watching
  // its tail arrive), and one that grew past the limit while open stays
  // expanded once it closes rather than snapping shut.
  const collapsible = lines > COLLAPSE_LINES && !svg && !diff && !open;
  const collapsed = collapsible && !expanded;
  useEffect(() => {
    if (open && lines > COLLAPSE_LINES) setExpanded(true);
  }, [open, lines]);
  return (
    <div
      className={`code-block${wrap ? " wrap" : ""}${collapsed ? " collapsed" : ""}`}
    >
      <div className="code-head">
        <span className="code-lang">{language || "text"}</span>
        {diagram?.svg && (
          <button
            type="button"
            className="ghost"
            aria-pressed={source}
            title="Show the diagram source"
            onClick={() => setSource((v) => !v)}
          >
            <Code size={14} />
            Source
          </button>
        )}
        {!svg && (
          <button
            type="button"
            className="ghost"
            aria-pressed={wrap}
            title="Wrap long lines"
            onClick={() => setWrap((v) => !v)}
          >
            <TextWrap size={14} />
            Wrap
          </button>
        )}
        <button type="button" className="ghost" onClick={copy}>
          {copied ? <Check size={14} /> : <Copy size={14} />}
          {copied ? "Copied" : "Copy"}
        </button>
      </div>
      {svg ? (
        <MermaidDiagram svg={svg} />
      ) : diff ? (
        <DiffView segments={diff} />
      ) : (
        <pre>{children}</pre>
      )}
      {diagram?.error && <p className="code-error">{diagram.error}</p>}
      {collapsible && (
        <button
          type="button"
          className="ghost code-expand"
          aria-expanded={!collapsed}
          onClick={() => setExpanded((v) => !v)}
        >
          {collapsed ? (
            <ChevronsUpDown size={14} />
          ) : (
            <ChevronsDownUp size={14} />
          )}
          {collapsed ? `Show all ${lines} lines` : "Collapse"}
        </button>
      )}
    </div>
  );
}
