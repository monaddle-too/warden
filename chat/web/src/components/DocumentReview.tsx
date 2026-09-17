import {
  Fragment,
  useEffect,
  useImperativeHandle,
  useRef,
  useState,
  type ReactNode,
  type Ref,
} from "react";
import { FileText } from "lucide-react";
import { api } from "../api";
import { parseInline, wordDiff, type Token } from "../docmarks";

/* Google Docs suggestions: the agent's proposal is immutable, the draft is
   what the owner reviews and Warden writes. Hunks are computed by the
   server from diff(base, draft); accept/reject and hand edits all change
   the draft. */

export type DocParagraph = {
  n: number;
  style: string;
  depth?: number;
  text: string;
  frozen?: string;
};
export type DocHunk = {
  id: number;
  from: number;
  to: number;
  after_from: number;
  after_to: number;
  removed: DocParagraph[];
  added: DocParagraph[];
  reasons?: string[];
};
type DocComment = { paragraph: number; text: string };
type DocConflict = {
  position: number;
  length: number;
  ours: DocParagraph[];
  theirs: DocParagraph[];
  base: DocParagraph[];
};
type Proposal = {
  document_id: string;
  title: string;
  url: string;
  base_revision: string;
  base: DocParagraph[];
  proposed: DocParagraph[];
  hunks: DocHunk[];
  summary: string;
  notes?: string[];
  rebased_from?: string;
  revises?: string;
};
type Review = {
  request_id: string;
  chatID: string;
  status: string;
  title: string;
  url: string;
  summary: string;
  changes: number;
  proposal?: Proposal;
  draft?: { paragraphs: DocParagraph[]; comments: DocComment[] };
  view?: {
    hunks: DocHunk[];
    rejected: { hunk: DocHunk; acceptable: boolean }[];
  };
  feedback?: string;
  error?: string;
  conflicts?: DocConflict[];
  revision_id?: string;
  written?: number;
};

export const DOC_STYLES = [
  "title",
  "subtitle",
  "h1",
  "h2",
  "h3",
  "h4",
  "h5",
  "h6",
  "text",
  "bullet",
  "numbered",
];
const awaiting = (status?: string) =>
  status === "pending" || status === "stale" || status === "applying";

function Rich({ text }: { text: string }) {
  return (
    <>
      {parseInline(text).map((run, i) => {
        let node: ReactNode = run.text;
        if (run.bold) node = <strong>{node}</strong>;
        if (run.italic) node = <em>{node}</em>;
        if (run.link)
          node = (
            <a href={run.link} target="_blank" rel="noreferrer">
              {node}
            </a>
          );
        return <Fragment key={i}>{node}</Fragment>;
      })}
    </>
  );
}

function TokenText({ token }: { token: Token }) {
  let node: ReactNode = token.text;
  if (token.bold) node = <strong>{node}</strong>;
  if (token.italic) node = <em>{node}</em>;
  if (token.link) node = <a href={token.link}>{node}</a>;
  return <>{node}</>;
}
function InlineDiff({
  before,
  after,
}: {
  before: DocParagraph;
  after: DocParagraph;
}) {
  const ops = wordDiff(before.text, after.text);
  const restyled =
    before.style !== after.style || (before.depth || 0) !== (after.depth || 0);
  return (
    <>
      <Paragraph paragraph={before} className="doc-removed">
        {ops
          .filter((o) => o.kind !== "+")
          .map((o, i) => (
            <span key={i} className={o.kind === "-" ? "doc-word-del" : ""}>
              <TokenText token={o.token} />
            </span>
          ))}
      </Paragraph>
      <Paragraph paragraph={after} className="doc-added">
        {ops
          .filter((o) => o.kind !== "-")
          .map((o, i) => (
            <span key={i} className={o.kind === "+" ? "doc-word-add" : ""}>
              <TokenText token={o.token} />
            </span>
          ))}
        {restyled && (
          <small className="doc-restyled">
            {styleLabel(before)} → {styleLabel(after)}
          </small>
        )}
      </Paragraph>
    </>
  );
}
const styleLabel = (p: DocParagraph) =>
  p.style === "bullet" || p.style === "numbered"
    ? `${p.style} · level ${(p.depth || 0) + 1}`
    : p.style;

/* One paragraph rendered as the document shows it. */
function Paragraph({
  paragraph,
  className = "",
  children,
}: {
  paragraph: DocParagraph;
  className?: string;
  children?: ReactNode;
}) {
  const p = paragraph;
  const body = children ?? (p.text ? <Rich text={p.text} /> : "\u00a0");
  const style = `doc-p doc-${p.style} ${p.frozen ? "doc-frozen" : ""} ${className}`;
  if (p.style === "bullet" || p.style === "numbered")
    return (
      <div
        className={style}
        style={{ marginLeft: 18 * (p.depth || 0) + 18 }}
        data-marker={p.style === "bullet" ? "•" : "#."}
      >
        {body}
      </div>
    );
  return <div className={style}>{body}</div>;
}

export type DocumentReviewHandle = { open: (id?: string) => void };
export type DocumentReviewState = { pending?: Review; local: Review[] };
export type { Review as DocumentProposal };

export function DocumentReview({
  ref,
  chatID,
  autoOpen = true,
  trigger,
  onState,
}: {
  ref?: Ref<DocumentReviewHandle>;
  chatID?: string;
  autoOpen?: boolean;
  trigger?: ((open: () => void) => ReactNode) | null;
  onState?: (state: DocumentReviewState) => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [reviews, setReviews] = useState<Review[]>([]);
  const [id, setID] = useState<string>();
  const [preview, setPreview] = useState<Review>();
  const [dismissed, setDismissed] = useState<string[]>([]);
  const [feedback, setFeedback] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [tab, setTab] = useState("suggestions");
  const [expanded, setExpanded] = useState<number[]>([]);
  useEffect(() => {
    let stopped = false;
    const refresh = async () => {
      try {
        const r = await api<{ requests: Review[] }>("sharing/doc_state");
        if (!stopped) setReviews(r.requests);
      } catch {
        /* Retry on next refresh. */
      }
    };
    void refresh();
    const timer = setInterval(refresh, 2000);
    return () => {
      stopped = true;
      clearInterval(timer);
    };
  }, []);
  const pending = reviews.find(
    (r) => r.chatID === chatID && awaiting(r.status),
  );
  const current = reviews.find((r) => r.request_id === id);
  const local = reviews.filter((r) => r.chatID === chatID);
  const report = useRef(onState);
  report.current = onState;
  const localKey = local.map((r) => r.request_id + ":" + r.status).join(",");
  useEffect(() => {
    report.current?.({ pending, local });
  }, [pending?.request_id, localKey]);
  useEffect(() => {
    if (autoOpen && pending && !dismissed.includes(pending.request_id))
      setID(pending.request_id);
  }, [autoOpen, pending?.request_id, dismissed]);
  const show = (which?: string) =>
    setID(which || (pending || local[local.length - 1])?.request_id);
  useImperativeHandle(ref, () => ({ open: show }));
  useEffect(() => {
    if (id) dialog.current?.showModal();
  }, [id]);
  const load = async (which: string) => {
    const r = await api<Review>(
      `sharing/doc_preview?id=${encodeURIComponent(which)}`,
    );
    setPreview(r);
    return r;
  };
  useEffect(() => {
    setPreview(undefined);
    setFeedback("");
    setError("");
    setTab("suggestions");
    setExpanded([]);
    if (!id) return;
    let stopped = false;
    void load(id).catch((e) => {
      if (!stopped) setError(String(e));
    });
    return () => {
      stopped = true;
    };
  }, [id]);
  // While Warden writes, follow the outcome.
  const status = current?.status || preview?.status;
  useEffect(() => {
    if (id && preview && current && current.status !== preview.status)
      void load(id).catch(() => {});
  }, [current?.status]);
  function close() {
    if (id) setDismissed((old) => [...old, id]);
    setID(undefined);
  }
  async function act(op: string, body: Record<string, unknown>) {
    if (!id) return;
    setBusy(true);
    setError("");
    try {
      const result = await api<Review>(`sharing/${op}`, { id, ...body });
      setPreview(result);
      setReviews((old) =>
        old.map((r) => (r.request_id === id ? { ...r, ...result } : r)),
      );
      return result;
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  const p = preview?.proposal,
    draft = preview?.draft,
    view = preview?.view && {
      hunks: preview.view.hunks ?? [],
      rejected: preview.view.rejected ?? [],
    };
  const editable = awaiting(status) && status !== "applying";
  /* Reject a current change: put the base paragraphs back in the draft. */
  const reject = (h: DocHunk) => {
    if (!p || !draft) return;
    const paragraphs = [
      ...draft.paragraphs.slice(0, h.after_from),
      ...p.base.slice(h.from, h.to),
      ...draft.paragraphs.slice(h.after_to),
    ];
    void act("doc_draft", { paragraphs: strip(paragraphs) });
  };
  return (
    <>
      {trigger === null ? null : trigger ? (
        trigger(() => show())
      ) : (
        <button
          className="chip"
          disabled={!local.length}
          onClick={() => show()}
        >
          <FileText size={13} />
          <span>Document suggestions</span>
        </button>
      )}
      {id && (
        <dialog
          ref={dialog}
          className="pr-review doc-review"
          onCancel={close}
          aria-labelledby="doc-title"
        >
          <header className="pr-header">
            <div>
              <span className={`pr-badge ${status}`}>
                {status === "pending"
                  ? "Awaiting your review"
                  : status === "stale"
                    ? "Document changed — review the merged draft"
                    : status === "applying"
                      ? "Writing to Google Docs…"
                      : status}
              </span>
              <p className="muted">
                {current?.title || p?.title}
                {(current?.url || p?.url) && (
                  <>
                    {" · "}
                    <a
                      href={current?.url || p?.url}
                      target="_blank"
                      rel="noreferrer"
                    >
                      Open in Google Docs ↗
                    </a>
                  </>
                )}
              </p>
              <h2 id="doc-title">
                {p?.summary || current?.summary || "Suggested edits"}
              </h2>
            </div>
            <button onClick={close} aria-label="Close document review">
              Close
            </button>
          </header>
          {local.length > 1 && (
            <label className="pr-history">
              Proposal history
              <select value={id} onChange={(e) => setID(e.target.value)}>
                {local.map((r) => (
                  <option key={r.request_id} value={r.request_id}>
                    {r.summary} · {r.status}
                  </option>
                ))}
              </select>
            </label>
          )}
          {error && (
            <p role="alert" className="error">
              {error}
            </p>
          )}
          {!p && !error && <p>Loading the saved proposal…</p>}
          {p && draft && view && (
            <>
              <nav className="pr-tabs" aria-label="Document review">
                <button
                  aria-pressed={tab === "suggestions"}
                  onClick={() => setTab("suggestions")}
                >
                  Suggestions <b>{view.hunks.length}</b>
                  {view.rejected.length > 0 && (
                    <span className="muted">
                      {" "}
                      · {view.rejected.length} rejected
                    </span>
                  )}
                </button>
                <button
                  aria-pressed={tab === "draft"}
                  onClick={() => setTab("draft")}
                >
                  Draft
                  {draft.comments.length > 0 && (
                    <span className="muted">
                      {" "}
                      · {draft.comments.length}{" "}
                      {draft.comments.length === 1 ? "comment" : "comments"}
                    </span>
                  )}
                </button>
                <button
                  aria-pressed={tab === "summary"}
                  onClick={() => setTab("summary")}
                >
                  Summary
                </button>
                <span>
                  <b className="pr-add">
                    +{view.hunks.reduce((n, h) => n + h.added.length, 0)}
                  </b>{" "}
                  <b className="pr-del">
                    −{view.hunks.reduce((n, h) => n + h.removed.length, 0)}
                  </b>{" "}
                  paragraphs
                </span>
              </nav>
              <section className="pr-content">
                {tab === "suggestions" && (
                  <Suggestions
                    base={p.base}
                    hunks={view.hunks}
                    rejected={view.rejected}
                    editable={editable && !busy}
                    expanded={expanded}
                    onExpand={(i) => setExpanded((old) => [...old, i])}
                    onReject={reject}
                    onAccept={(h) =>
                      void act("doc_decide", { hunk: h.id, accept: true })
                    }
                  />
                )}
                {tab === "draft" && (
                  <DraftEditor
                    key={preview?.request_id + ":" + status}
                    paragraphs={draft.paragraphs}
                    comments={draft.comments}
                    editable={editable && !busy}
                    onSave={(paragraphs, comments) =>
                      void act("doc_draft", {
                        paragraphs: strip(paragraphs),
                        comments,
                      })
                    }
                  />
                )}
                {tab === "summary" && (
                  <div className="pr-description doc-summary">
                    <h3>Agent's summary</h3>
                    <p>{p.summary}</p>
                    {p.notes?.map((note, i) => (
                      <p key={i} className="muted">
                        {note}
                      </p>
                    ))}
                    {p.rebased_from && (
                      <p className="muted">
                        The agent read revision {p.rebased_from.slice(0, 12)}…;
                        the draft was merged onto the document's current
                        revision.
                      </p>
                    )}
                    {preview?.conflicts && preview.conflicts.length > 0 && (
                      <>
                        <h3>Conflicts with the current document</h3>
                        {preview.conflicts.map((c, i) => (
                          <div key={i} className="doc-conflict">
                            <p className="muted">
                              At paragraph {c.position}: the draft keeps the
                              document's current text. The proposal had:
                            </p>
                            {c.ours.map((q, j) => (
                              <Paragraph
                                key={j}
                                paragraph={q}
                                className="doc-removed"
                              />
                            ))}
                            {!c.ours.length && (
                              <p className="muted">(deleted)</p>
                            )}
                          </div>
                        ))}
                      </>
                    )}
                    {preview?.error && (
                      <p role="alert" className="error">
                        {preview.error}
                      </p>
                    )}
                    {status === "applied" && (
                      <p>
                        Written to the document
                        {preview?.written ? ` in ${preview.written} edits` : ""}
                        .
                        {preview?.revision_id && (
                          <> New revision {preview.revision_id.slice(0, 12)}…</>
                        )}
                      </p>
                    )}
                    {preview?.feedback &&
                      (status === "rejected" || status === "returned") && (
                        <p>Feedback sent: {preview.feedback}</p>
                      )}
                  </div>
                )}
              </section>
            </>
          )}
          {editable && p && (
            <footer className="pr-footer">
              <label>
                Feedback for the agent (optional)
                <textarea
                  maxLength={4000}
                  value={feedback}
                  onChange={(e) => setFeedback(e.target.value)}
                  placeholder="What should change? Sent with Reject or Send back."
                />
              </label>
              <div>
                <p>
                  Approval writes the draft as it stands to the Google Doc with
                  your account. Nothing is written until then.
                </p>
                <button
                  disabled={busy}
                  onClick={() => void act("doc_rebase", {})}
                  title="Read the document again and merge the draft onto it"
                >
                  Refresh from Google
                </button>
                <button
                  disabled={busy}
                  onClick={() =>
                    void act("doc_resolve", { allow: false, feedback }).then(
                      close,
                    )
                  }
                >
                  Reject
                </button>
                <button
                  disabled={busy}
                  onClick={() =>
                    void act("doc_return", { feedback }).then(close)
                  }
                  title="Send the draft and your comments to the agent for another round"
                >
                  Send back to agent
                </button>
                <button
                  className="pr-publish"
                  disabled={busy}
                  onClick={() => void act("doc_resolve", { allow: true })}
                >
                  Approve and write to Google Doc
                </button>
              </div>
            </footer>
          )}
          {status === "applying" && (
            <p role="status">
              Writing the reviewed draft to the Google Doc… You can close this
              review; Warden will notify the agent when it finishes.
            </p>
          )}
        </dialog>
      )}
    </>
  );
}

/* The server rejects paragraph fields it does not know. */
function strip(paragraphs: DocParagraph[]) {
  return paragraphs.map((p) => ({
    style: p.style,
    depth: p.depth || 0,
    text: p.text,
    ...(p.frozen ? { frozen: p.frozen } : {}),
  }));
}

/* The document with its changes inline, unchanged stretches folded. */
function Suggestions({
  base,
  hunks,
  rejected,
  editable,
  expanded,
  onExpand,
  onReject,
  onAccept,
}: {
  base: DocParagraph[];
  hunks: DocHunk[];
  rejected: { hunk: DocHunk; acceptable: boolean }[];
  editable: boolean;
  expanded: number[];
  onExpand: (i: number) => void;
  onReject: (h: DocHunk) => void;
  onAccept: (h: DocHunk) => void;
}) {
  const context = 1;
  const parts: ReactNode[] = [];
  let pos = 0;
  const fold = (from: number, to: number, key: number) => {
    if (to <= from) return;
    if (to - from <= context * 2 + 1 || expanded.includes(key)) {
      for (let i = from; i < to; i++)
        parts.push(<Paragraph key={"b" + i} paragraph={base[i]} />);
      return;
    }
    for (let i = from; i < from + context; i++)
      parts.push(<Paragraph key={"b" + i} paragraph={base[i]} />);
    parts.push(
      <button
        key={"fold" + key}
        className="doc-fold"
        onClick={() => onExpand(key)}
      >
        … {to - from - context * 2} unchanged paragraphs
      </button>,
    );
    for (let i = to - context; i < to; i++)
      parts.push(<Paragraph key={"b" + i} paragraph={base[i]} />);
  };
  // A rejected suggestion sits above the base paragraphs it would have
  // replaced, which the fold below it shows as they are.
  const rejectedBlock = (r: { hunk: DocHunk; acceptable: boolean }) => (
    <div key={"r" + r.hunk.id} className="doc-hunk doc-hunk-rejected">
      <div className="doc-hunk-bar">
        <span>
          Rejected suggestion
          {r.hunk.removed.length > 0 &&
            ` (would ${r.hunk.added.length ? "replace" : "delete"} the ${r.hunk.removed.length === 1 ? "paragraph" : r.hunk.removed.length + " paragraphs"} below)`}
        </span>
        {r.hunk.reasons?.map((reason, i) => (
          <em key={i}>{reason}</em>
        ))}
        {editable && (
          <button
            disabled={!r.acceptable}
            title={r.acceptable ? "" : "Those paragraphs were edited by hand"}
            onClick={() => onAccept(r.hunk)}
          >
            Accept
          </button>
        )}
      </div>
      {r.hunk.added.map((q, i) => (
        <Paragraph
          key={"y" + i}
          paragraph={q}
          className="doc-added doc-muted"
        />
      ))}
    </div>
  );
  // Rejected suggestions are placed where their base range starts; hunks
  // and rejected blocks are walked together in base order.
  const events = [
    ...hunks.map((h) => ({ at: h.from, hunk: h })),
    ...rejected.map((r) => ({ at: r.hunk.from, rejected: r })),
  ].sort((a, b) => a.at - b.at);
  let key = 0;
  for (const event of events) {
    fold(pos, event.at, key++);
    pos = Math.max(pos, event.at);
    if ("rejected" in event && event.rejected) {
      parts.push(rejectedBlock(event.rejected));
      continue;
    }
    const h = (event as { hunk: DocHunk }).hunk;
    parts.push(
      <div key={"h" + h.id} className="doc-hunk">
        <div className="doc-hunk-bar">
          <span>
            {h.removed.length && h.added.length
              ? "Changed"
              : h.added.length
                ? "Added"
                : "Deleted"}
          </span>
          {h.reasons?.map((reason, i) => (
            <em key={i}>{reason}</em>
          ))}
          {editable && <button onClick={() => onReject(h)}>Reject</button>}
        </div>
        {h.removed.length === 1 && h.added.length === 1 ? (
          <InlineDiff before={h.removed[0]} after={h.added[0]} />
        ) : (
          <>
            {h.removed.map((q, i) => (
              <Paragraph key={"x" + i} paragraph={q} className="doc-removed" />
            ))}
            {h.added.map((q, i) => (
              <Paragraph key={"y" + i} paragraph={q} className="doc-added" />
            ))}
          </>
        )}
      </div>,
    );
    pos = h.to;
  }
  fold(pos, base.length, key++);
  return (
    <div className="doc-page" aria-label="Suggested changes">
      {parts}
      {!hunks.length && (
        <p className="muted">
          The draft matches the document. Accept a rejected suggestion or edit
          the draft, or reject the proposal.
        </p>
      )}
    </div>
  );
}

/* The draft as editable paragraphs, saved as a whole. */
function DraftEditor({
  paragraphs,
  comments,
  editable,
  onSave,
}: {
  paragraphs: DocParagraph[];
  comments: DocComment[];
  editable: boolean;
  onSave: (paragraphs: DocParagraph[], comments: DocComment[]) => void;
}) {
  const [items, setItems] = useState(paragraphs);
  const [notes, setNotes] = useState(comments);
  const [dirty, setDirty] = useState(false);
  const [commenting, setCommenting] = useState<number>();
  const update = (i: number, changes: Partial<DocParagraph>) => {
    setItems((old) => old.map((p, j) => (j === i ? { ...p, ...changes } : p)));
    setDirty(true);
  };
  const insertAfter = (i: number) => {
    setItems((old) => [
      ...old.slice(0, i + 1),
      { n: 0, style: "text", text: "" },
      ...old.slice(i + 1),
    ]);
    setNotes((old) =>
      old.map((c) =>
        c.paragraph > i + 1 ? { ...c, paragraph: c.paragraph + 1 } : c,
      ),
    );
    setDirty(true);
  };
  const remove = (i: number) => {
    setItems((old) => old.filter((_, j) => j !== i));
    setNotes((old) =>
      old
        .filter((c) => c.paragraph !== i + 1)
        .map((c) =>
          c.paragraph > i + 1 ? { ...c, paragraph: c.paragraph - 1 } : c,
        ),
    );
    setDirty(true);
  };
  const save = () => {
    onSave(items, notes);
    setDirty(false);
  };
  return (
    <div className="doc-draft">
      <p className="muted">
        Edit the draft directly; text uses **bold**, *italic* and [link](url).
        Comments go to the agent with “Send back”.
        {editable && (
          <button className="doc-save" disabled={!dirty} onClick={save}>
            Save draft
          </button>
        )}
      </p>
      {items.map((p, i) => (
        <div key={i} className={`doc-row ${p.frozen ? "doc-row-frozen" : ""}`}>
          <span className="doc-row-n">{i + 1}</span>
          {p.frozen ? (
            <Paragraph paragraph={p} />
          ) : (
            <>
              <select
                aria-label={`Paragraph ${i + 1} style`}
                value={p.style}
                disabled={!editable}
                onChange={(e) =>
                  update(i, {
                    style: e.target.value,
                    depth:
                      e.target.value === "bullet" ||
                      e.target.value === "numbered"
                        ? p.depth || 0
                        : 0,
                  })
                }
              >
                {DOC_STYLES.map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </select>
              {p.style === "bullet" || p.style === "numbered" ? (
                <input
                  type="number"
                  aria-label={`Paragraph ${i + 1} list level`}
                  min={1}
                  max={9}
                  value={(p.depth || 0) + 1}
                  disabled={!editable}
                  onChange={(e) =>
                    update(i, {
                      depth: Math.max(
                        0,
                        Math.min(8, Number(e.target.value) - 1),
                      ),
                    })
                  }
                />
              ) : (
                <span />
              )}
              <textarea
                aria-label={`Paragraph ${i + 1}`}
                className={`doc-${p.style}`}
                value={p.text}
                rows={Math.max(1, Math.ceil(p.text.length / 90))}
                disabled={!editable}
                onChange={(e) =>
                  update(i, { text: e.target.value.replace(/[\r\n]+/g, " ") })
                }
              />
            </>
          )}
          {editable && (
            <span className="doc-row-actions">
              <button
                onClick={() => setCommenting(commenting === i ? undefined : i)}
                title="Comment for the agent"
              >
                💬
              </button>
              <button
                onClick={() => insertAfter(i)}
                title="Insert a paragraph after"
              >
                +
              </button>
              {!p.frozen && (
                <button onClick={() => remove(i)} title="Delete this paragraph">
                  ×
                </button>
              )}
            </span>
          )}
          {(commenting === i || notes.some((c) => c.paragraph === i + 1)) && (
            <div className="doc-comments">
              {notes
                .map((c, k) => ({ c, k }))
                .filter(({ c }) => c.paragraph === i + 1)
                .map(({ c, k }) => (
                  <div key={k} className="doc-comment">
                    <textarea
                      aria-label={`Comment on paragraph ${i + 1}`}
                      value={c.text}
                      disabled={!editable}
                      onChange={(e) => {
                        setNotes((old) =>
                          old.map((x, j) =>
                            j === k ? { ...x, text: e.target.value } : x,
                          ),
                        );
                        setDirty(true);
                      }}
                    />
                    {editable && (
                      <button
                        onClick={() => {
                          setNotes((old) => old.filter((_, j) => j !== k));
                          setDirty(true);
                        }}
                      >
                        Remove
                      </button>
                    )}
                  </div>
                ))}
              {commenting === i && editable && (
                <button
                  onClick={() => {
                    setNotes((old) => [...old, { paragraph: i + 1, text: "" }]);
                    setDirty(true);
                  }}
                >
                  Add comment
                </button>
              )}
            </div>
          )}
        </div>
      ))}
      {editable && items.length === 0 && (
        <button onClick={() => insertAfter(-1)}>Add a paragraph</button>
      )}
    </div>
  );
}
