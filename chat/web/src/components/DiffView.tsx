import { useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { COLLAPSE_LINES } from "../code";
import type { DiffFile, DiffHunk, DiffSegment } from "../diff";

const MARK = { add: "+", del: "-", ctx: " ", meta: "\\" } as const;

/* A unified diff as parsed by diff.ts: prose segments stay <pre>, each file
   gets a header with its path and counts, each hunk a header that folds it.
   Line numbers and the +/- marks are CSS content from data attributes, so a
   copied selection is the code alone. Everything rendered is a text node. */
export function DiffView({ segments }: { segments: DiffSegment[] }) {
  return (
    <div className="diff-view">
      {segments.map((segment, i) =>
        segment.kind === "text" ? (
          <pre key={i}>{segment.text}</pre>
        ) : (
          <File key={i} file={segment.file} />
        ),
      )}
    </div>
  );
}

function File({ file }: { file: DiffFile }) {
  // A fence written by hand has no `@@` headers and so no numbers to show.
  const numbered = file.hunks.some((h) => h.header !== "");
  return (
    <div className="diff-file">
      {(file.path || file.status) && (
        <div className="diff-head">
          <span className="diff-path">{file.path || "file"}</span>
          {file.status && <span className="diff-status">{file.status}</span>}
          <span className="diff-count">
            <b className="diff-plus">+{file.additions}</b>{" "}
            <b className="diff-minus">−{file.deletions}</b>
          </span>
        </div>
      )}
      {file.hunks.map((hunk, i) => (
        <Hunk key={i} hunk={hunk} numbered={numbered} />
      ))}
    </div>
  );
}

function Hunk({ hunk, numbered }: { hunk: DiffHunk; numbered: boolean }) {
  const long = hunk.lines.length > COLLAPSE_LINES;
  const [open, setOpen] = useState(!long);
  // A headerless hunk only gets a fold row when it is long enough to need one.
  const foldable = hunk.header !== "" || long;
  return (
    <div className="diff-hunk">
      {foldable && (
        <button
          type="button"
          className="ghost diff-fold"
          aria-expanded={open}
          onClick={() => setOpen((v) => !v)}
        >
          {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
          <span>{hunk.header || `${hunk.lines.length} lines`}</span>
          {!open && hunk.header && (
            <span className="muted">{hunk.lines.length} lines</span>
          )}
        </button>
      )}
      {(open || !foldable) &&
        hunk.lines.map((line, i) => (
          <div key={i} className={`diff-line diff-${line.kind}`}>
            {numbered && (
              <span className="diff-no" data-no={line.oldNo ?? ""} />
            )}
            {numbered && (
              <span className="diff-no" data-no={line.newNo ?? ""} />
            )}
            <code data-mark={MARK[line.kind]}>{line.text || " "}</code>
          </div>
        ))}
    </div>
  );
}
