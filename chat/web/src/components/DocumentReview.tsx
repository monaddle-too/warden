import {
  Suspense,
  lazy,
  useEffect,
  useImperativeHandle,
  useRef,
  useState,
  type ReactNode,
  type Ref,
} from "react";
import { FileText } from "lucide-react";
import { api } from "../api";
import type { DocComment, SuggestionCard } from "../documents/suggestions";

/* Google Docs suggestions: the agent's proposal is immutable, the draft is
   what the owner reviews and Warden writes. The page is a document the
   server computes from diff(base, draft) with the changes as suggestions;
   every decision and edit goes back to the server, which returns a fresh
   page. The editor itself is Panta's suggestion layer, loaded on first use. */

const SuggestionEditor = lazy(() =>
  import("../documents/suggestions").then((m) => ({
    default: m.SuggestionEditor,
  })),
);

export type DocParagraph = {
  n: number;
  style: string;
  depth?: number;
  text: string;
  frozen?: string;
};
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
    document: Record<string, unknown>;
    suggestions: SuggestionCard[];
    comments: DocComment[];
  };
  feedback?: string;
  error?: string;
  conflicts?: DocConflict[];
  revision_id?: string;
  written?: number;
};

const awaiting = (status?: string) =>
  status === "pending" || status === "stale" || status === "applying";

const plain = (paragraphs: DocParagraph[]) =>
  paragraphs.map((p) => p.text.replace(/\\([\\*[\]])/g, "$1")).join(" / ") ||
  "(nothing)";

export type DocumentReviewHandle = { open: (id?: string) => void };
export type DocumentReviewState = { pending?: Review; local: Review[] };
export type { Review as DocumentProposal };

export function DocumentReview({
  ref,
  chatID,
  author = "The agent",
  autoOpen = true,
  trigger,
  onState,
}: {
  ref?: Ref<DocumentReviewHandle>;
  chatID?: string;
  author?: string;
  autoOpen?: boolean;
  trigger?: ((open: () => void) => ReactNode) | null;
  onState?: (state: DocumentReviewState) => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [reviews, setReviews] = useState<Review[]>([]);
  const [id, setID] = useState<string>();
  const [preview, setPreview] = useState<Review>();
  const [revision, setRevision] = useState(0);
  const [dismissed, setDismissed] = useState<string[]>([]);
  const [feedback, setFeedback] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [tab, setTab] = useState("review");
  // Server calls for one review run one at a time, in order: a debounced
  // page save must land before the decision that follows it.
  const queue = useRef(Promise.resolve());
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
  const receive = (r: Review) => {
    setPreview(r);
    setRevision((n) => n + 1);
  };
  const load = async (which: string) => {
    const r = await api<Review>(
      `sharing/doc_preview?id=${encodeURIComponent(which)}`,
    );
    receive(r);
    return r;
  };
  useEffect(() => {
    setPreview(undefined);
    setFeedback("");
    setError("");
    setTab("review");
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
  function act(op: string, body: Record<string, unknown>) {
    if (!id) return Promise.resolve(undefined);
    const which = id;
    const run = queue.current.then(async () => {
      setBusy(true);
      setError("");
      try {
        const result = await api<Review>(`sharing/${op}`, {
          id: which,
          ...body,
        });
        receive(result);
        setReviews((old) =>
          old.map((r) => (r.request_id === which ? { ...r, ...result } : r)),
        );
        return result;
      } catch (e) {
        setError(String(e));
        return undefined;
      } finally {
        setBusy(false);
      }
    });
    queue.current = run.then(() => undefined);
    return run;
  }
  const p = preview?.proposal,
    view = preview?.view;
  const editable = awaiting(status) && status !== "applying";
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
          {p && view && (
            <>
              <nav className="pr-tabs" aria-label="Document review">
                <button
                  aria-pressed={tab === "review"}
                  onClick={() => setTab("review")}
                >
                  Suggestions{" "}
                  <b>
                    {
                      view.suggestions.filter((s) => s.status === "pending")
                        .length
                    }
                  </b>
                </button>
                <button
                  aria-pressed={tab === "summary"}
                  onClick={() => setTab("summary")}
                >
                  Summary
                </button>
                <span>
                  {view.comments.length > 0 &&
                    `${view.comments.length} comment${view.comments.length === 1 ? "" : "s"}`}
                </span>
              </nav>
              <section className="pr-content doc-content">
                {tab === "review" && (
                  <Suspense
                    fallback={<p className="muted">Loading the editor…</p>}
                  >
                    <SuggestionEditor
                      document={view.document}
                      revision={revision}
                      suggestions={view.suggestions}
                      comments={view.comments}
                      editable={editable}
                      busy={busy}
                      author={author}
                      onSave={(document) => void act("doc_draft", { document })}
                      onAccept={(change) =>
                        void act("doc_decide", { change, accept: true })
                      }
                      onReject={(change) =>
                        void act("doc_decide", { change, accept: false })
                      }
                      onRestore={(hunk) =>
                        void act("doc_decide", { hunk, accept: true })
                      }
                      onComments={(comments) =>
                        void act("doc_draft", { comments })
                      }
                    />
                  </Suspense>
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
                        The agent read revision {p.rebased_from.slice(0, 12)}
                        …; the draft was merged onto the document's current
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
                              document's current text, “{plain(c.theirs)}”. The
                              proposal had “{plain(c.ours)}”.
                            </p>
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
                  Approval writes the page as it stands to the Google Doc with
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
