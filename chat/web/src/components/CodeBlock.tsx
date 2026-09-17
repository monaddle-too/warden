import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
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
import { isMermaidFence } from "../mermaid";
import { MermaidDiagram, useMermaid } from "./Mermaid";

/* A fenced block from the transcript: language label, copy, wrap toggle and
   a collapse for long output. `children` is the <code> react-markdown already
   rendered (highlighted spans included); `node` is its hast source, which is
   where the text for the clipboard and the line count come from. A closed
   ```mermaid fence becomes a diagram with a toggle back to its source. */
export function CodeBlock({
  node,
  streaming,
  children,
}: {
  node?: HastNode;
  streaming?: boolean;
  children?: ReactNode;
}) {
  const code = codeChild(node);
  const language = codeLanguage(code?.properties?.className);
  const text = useMemo(() => hastText(code), [code]);
  const lines = lineCount(text);
  const [wrap, setWrap] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const [copied, setCopied] = useState(false);
  const [source, setSource] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  // While the entry streams the fence may still be open, so the diagram waits
  // for the final text; the source stays visible until it renders.
  const diagram = useMermaid(text, isMermaidFence(language) && !streaming);
  const svg = source ? undefined : diagram?.svg;
  const collapsible = lines > COLLAPSE_LINES && !svg;
  const collapsed = collapsible && !expanded;
  const copy = () => {
    // The clipboard is unavailable outside secure contexts; the button then
    // simply stays "Copy" and the user selects the text by hand.
    void navigator.clipboard?.writeText(text).then(
      () => {
        setCopied(true);
        clearTimeout(timer.current);
        timer.current = setTimeout(() => setCopied(false), 1500);
      },
      () => {},
    );
  };
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
      {svg ? <MermaidDiagram svg={svg} /> : <pre>{children}</pre>}
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
