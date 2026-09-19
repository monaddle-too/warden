import {
  useEffect,
  useImperativeHandle,
  useRef,
  useState,
  type ReactNode,
  type Ref,
} from "react";
import { GitPullRequest } from "lucide-react";
import { ImageAttachment } from "./ImageAttachment";
import ReactMarkdown from "react-markdown";
import { api } from "../api";

type FileChange = {
  path: string;
  change: string;
  diff: string[];
  additions: number;
  deletions: number;
};
type Proposal = {
  images?: { image_id: string; caption: string; url: string; path: string }[];
  title: string;
  body: string;
  repository: string;
  base: string;
  base_sha: string;
  head: string;
  files: FileChange[];
  /* An update to a pull request Warden published: the commit lands on
     head (the pull request's branch) instead of opening a new one. */
  pull_request?: { number: number; base: string; url: string };
};
type Review = {
  request_id: string;
  chatID: string;
  status: string;
  title: string;
  repository: string;
  proposal?: Proposal;
  feedback?: string;
  error?: string;
  url?: string;
  branch_url?: string;
};
function Diff({ file }: { file: FileChange }) {
  let oldLine = 0,
    newLine = 0;
  return (
    <details className="pr-file" open>
      <summary>
        <strong>{file.path}</strong>
        <span>
          {file.change} <b className="pr-add">+{file.additions}</b>{" "}
          <b className="pr-del">−{file.deletions}</b>
        </span>
      </summary>
      <div
        className="pr-diff"
        role="region"
        aria-label={`Diff for ${file.path}`}
        tabIndex={0}
      >
        {file.diff.map((line, i) => {
          const hunk = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line);
          if (hunk) {
            oldLine = Number(hunk[1]);
            newLine = Number(hunk[2]);
          }
          const header = hunk || i < 2 || line.startsWith("\\");
          const added = !header && line.startsWith("+"),
            removed = !header && line.startsWith("-");
          const old = header || added ? "" : oldLine++,
            next = header || removed ? "" : newLine++;
          return (
            <div
              key={i}
              className={`pr-line ${hunk ? "pr-hunk" : added ? "pr-added" : removed ? "pr-removed" : ""}`}
            >
              <span className="pr-line-number">{old}</span>
              <span className="pr-line-number">{next}</span>
              <code>{line.replaceAll("\r", "␍") || " "}</code>
            </div>
          );
        })}
        {!file.diff.length && <p>Empty file {file.change}.</p>}
      </div>
    </details>
  );
}
export type PullRequestReviewHandle = { open: (id?: string) => void };
export type PullRequestState = { pending?: Review; local: Review[] };
export type { Review as PullRequestProposal };
/* The proposal review dialog. Pass `trigger={null}` and drive it through the
   ref to review from a transcript card or the workspace panel. */
export function PullRequestReview({
  ref,
  chatID,
  autoOpen = true,
  trigger,
  onState,
}: {
  ref?: Ref<PullRequestReviewHandle>;
  chatID?: string;
  autoOpen?: boolean;
  trigger?: ((open: () => void) => ReactNode) | null;
  onState?: (state: PullRequestState) => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [reviews, setReviews] = useState<Review[]>([]);
  const [id, setID] = useState<string>();
  const [preview, setPreview] = useState<Review>();
  const [dismissed, setDismissed] = useState<string[]>([]);
  const [body, setBody] = useState("");
  const [feedback, setFeedback] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [tab, setTab] = useState("files");
  useEffect(() => {
    let stopped = false;
    const refresh = async () => {
      try {
        const r = await api<{ requests: Review[] }>("sharing/pr_state");
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
    (r) => r.chatID === chatID && r.status === "pending",
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
  useEffect(() => {
    setPreview(undefined);
    setFeedback("");
    setError("");
    setTab("files");
    if (!id) return;
    let stopped = false;
    void api<Review>(`sharing/pr_preview?id=${encodeURIComponent(id)}`)
      .then((r) => {
        if (!stopped) {
          setPreview(r);
          setBody(r.proposal?.body || "");
        }
      })
      .catch((e) => {
        if (!stopped) setError(String(e));
      });
    return () => {
      stopped = true;
    };
  }, [id]);
  function close() {
    if (id) setDismissed((old) => [...old, id]);
    setID(undefined);
  }
  async function resolve(allow: boolean) {
    if (!id) return;
    setBusy(true);
    setError("");
    try {
      const result = await api<Review>("sharing/pr_resolve", {
        id,
        allow,
        feedback,
        body,
      });
      setReviews((old) => old.map((r) => (r.request_id === id ? result : r)));
      if (!allow) close();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  const p = preview?.proposal,
    status = current?.status || preview?.status;
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
          <GitPullRequest size={13} />
          <span>Pull requests</span>
        </button>
      )}
      {id && (
        <dialog
          ref={dialog}
          className="pr-review"
          onCancel={close}
          aria-labelledby="pr-title"
        >
          <header className="pr-header">
            <div>
              <span className={`pr-badge ${status}`}>
                {status === "pending" ? "Awaiting your review" : status}
              </span>
              <p className="muted">{current?.repository}</p>
              <h2 id="pr-title">
                {p?.title || current?.title || "Pull request proposal"}
              </h2>
            </div>
            <button onClick={close} aria-label="Close pull request review">
              Close
            </button>
          </header>
          {local.length > 1 && (
            <label className="pr-history">
              Proposal history
              <select value={id} onChange={(e) => setID(e.target.value)}>
                {local.map((r) => (
                  <option key={r.request_id} value={r.request_id}>
                    {r.title} · {r.status}
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
          {p && (
            <>
              {p.pull_request ? (
                <p className="pr-branches">
                  Update to{" "}
                  <a href={p.pull_request.url} target="_blank" rel="noreferrer">
                    #{p.pull_request.number}
                  </a>
                  : a commit onto <code>{p.head}</code>
                  <span>Current head {p.base_sha.slice(0, 12)}</span>
                </p>
              ) : (
                <p className="pr-branches">
                  <code>{p.base}</code> ← <code>{p.head}</code>
                  <span>Base commit {p.base_sha.slice(0, 12)}</span>
                </p>
              )}
              <nav className="pr-tabs" aria-label="Pull request preview">
                <button
                  aria-pressed={tab === "description"}
                  onClick={() => setTab("description")}
                >
                  Description
                </button>
                <button
                  aria-pressed={tab === "files"}
                  onClick={() => setTab("files")}
                >
                  Files changed{" "}
                  <b>{p.files.length + (p.images?.length || 0)}</b>
                </button>
                <span>
                  <b className="pr-add">
                    +{p.files.reduce((n, f) => n + f.additions, 0)}
                  </b>{" "}
                  <b className="pr-del">
                    −{p.files.reduce((n, f) => n + f.deletions, 0)}
                  </b>
                </span>
              </nav>
              <section className="pr-content">
                {tab === "description" ? (
                  <div className="pr-description">
                    {status === "pending" && (
                      <label className="pr-body-editor">
                        Pull request body (Markdown)
                        <textarea
                          aria-label="Pull request body"
                          value={body}
                          disabled={busy}
                          onChange={(e) => setBody(e.target.value)}
                        />
                        <span className="muted">
                          Your edits below are the body Warden will send when
                          you approve.
                        </span>
                      </label>
                    )}
                    <h3>Body preview</h3>
                    <ReactMarkdown
                      components={{
                        img: ({ alt, src }) => {
                          const image = p.images?.find((i) => i.url === src);
                          return image && preview ? (
                            <ImageAttachment
                              chatID={preview.chatID}
                              id={image.image_id}
                              caption={image.caption}
                            />
                          ) : (
                            <span>{alt}</span>
                          );
                        },
                        a: ({ href, children }) => (
                          <a href={href} target="_blank" rel="noreferrer">
                            {children}
                          </a>
                        ),
                      }}
                    >
                      {body || "No description provided."}
                    </ReactMarkdown>
                  </div>
                ) : (
                  <>
                    {p.files.map((f) => (
                      <Diff key={f.path} file={f} />
                    ))}
                    {p.images?.map((i) => (
                      <section key={i.image_id} className="pr-file">
                        <p>{i.path} · Added image</p>
                        <ImageAttachment
                          chatID={preview!.chatID}
                          id={i.image_id}
                          caption={i.caption}
                        />
                      </section>
                    ))}
                  </>
                )}
              </section>
            </>
          )}
          {current?.url && (
            <p>
              <a href={current.url} target="_blank" rel="noreferrer">
                Open published pull request on GitHub ↗
              </a>
            </p>
          )}
          {current?.error && (
            <p role="alert" className="error">
              {current.error}{" "}
              {current.branch_url && (
                <a href={current.branch_url} target="_blank" rel="noreferrer">
                  Check proposal branch on GitHub
                </a>
              )}
            </p>
          )}
          {current?.feedback && status === "rejected" && (
            <p>Feedback: {current.feedback}</p>
          )}
          {status === "pending" && (
            <footer className="pr-footer">
              <label>
                Feedback for the agent (optional)
                <textarea
                  maxLength={4000}
                  value={feedback}
                  onChange={(e) => setFeedback(e.target.value)}
                  placeholder="What should change before this is ready?"
                />
              </label>
              <div>
                <p>
                  Approval publishes this exact proposal as a new branch and
                  pull request.
                </p>
                <button
                  disabled={busy || !p}
                  onClick={() => void resolve(false)}
                >
                  Reject and continue chatting
                </button>
                <button
                  className="pr-publish"
                  disabled={busy || !p}
                  onClick={() => void resolve(true)}
                >
                  Approve and create pull request
                </button>
              </div>
            </footer>
          )}
          {status === "publishing" && (
            <p role="status">
              Creating the reviewed branch and pull request on GitHub… You can
              close this preview; Warden will notify the agent when it finishes.
            </p>
          )}
        </dialog>
      )}
    </>
  );
}
