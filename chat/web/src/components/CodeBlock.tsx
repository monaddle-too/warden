import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  Check,
  ChevronsDownUp,
  ChevronsUpDown,
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

/* A fenced block from the transcript: language label, copy, wrap toggle and
   a collapse for long output. `children` is the <code> react-markdown already
   rendered (highlighted spans included); `node` is its hast source, which is
   where the text for the clipboard and the line count come from. */
export function CodeBlock({
  node,
  children,
}: {
  node?: HastNode;
  children?: ReactNode;
}) {
  const code = codeChild(node);
  const language = codeLanguage(code?.properties?.className);
  const text = useMemo(() => hastText(code), [code]);
  const lines = lineCount(text);
  const [wrap, setWrap] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const collapsible = lines > COLLAPSE_LINES;
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
        <button type="button" className="ghost" onClick={copy}>
          {copied ? <Check size={14} /> : <Copy size={14} />}
          {copied ? "Copied" : "Copy"}
        </button>
      </div>
      <pre>{children}</pre>
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
