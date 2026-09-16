import {
  useEffect,
  useImperativeHandle,
  useRef,
  useState,
  type ReactNode,
  type Ref,
} from "react";
import { FileText } from "lucide-react";
import { api } from "../api";

type Doc = { id: string; title: string; url: string };
export type DocumentRequest = {
  request_id: string;
  chatID: string;
  sandboxID: string;
  reason: string;
  access: "read" | "write" | "create";
  title: string;
  status: string;
  expires_at: number | null;
  documents: Doc[];
};
type File = { id: string; name: string; blocked?: boolean };
export type DocumentSharingHandle = {
  open: () => void;
  decline: () => void;
};
export type DocumentSharingState = {
  pending?: DocumentRequest;
  grants: DocumentRequest[];
};
/* The picker dialog for Google documents. The default trigger is a chip; a
   host can pass `trigger={null}` and drive it through the ref instead, so
   agent requests can be answered from a card in the transcript. */
export function DocumentSharing({
  ref,
  chatID,
  sandboxID,
  siblings = [],
  canConnectGoogle = true,
  autoOpen = true,
  trigger,
  onState,
}: {
  ref?: Ref<DocumentSharingHandle>;
  chatID?: string;
  sandboxID?: string;
  siblings?: string[];
  canConnectGoogle?: boolean;
  autoOpen?: boolean;
  trigger?: ((open: () => void) => ReactNode) | null;
  onState?: (state: DocumentSharingState) => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [requests, setRequests] = useState<DocumentRequest[]>([]);
  const [status, setStatus] = useState<{
    configured: boolean;
    connected: boolean;
    can_write: boolean;
    // google.configured says whether providers.google exists at all; the
    // top-level flags describe the client and the connected account.
    google?: { configured: boolean; connected: boolean };
  }>({
    configured: false,
    connected: false,
    can_write: false,
  });
  const [open, setOpen] = useState(false);
  useEffect(() => {
    if (open) dialog.current?.showModal();
  }, [open]);
  const [dismissed, setDismissed] = useState<string[]>([]);
  const [files, setFiles] = useState<File[]>([]);
  const [page, setPage] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [selecting, setSelecting] = useState(false);
  const [access, setAccess] = useState("read");
  const [duration, setDuration] = useState(3600);
  const [query, setQuery] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const pending = requests.find(
    (r) => r.status === "pending" && (!chatID || r.chatID === chatID),
  );
  const needsWrite =
    pending?.access === "create" ||
    pending?.access === "write" ||
    (selecting && access === "write");
  const ready = status.connected && (!needsWrite || status.can_write);
  // Grants belong to the workspace: every chat on the sandbox can use them.
  const grants = requests.filter(
    (r) =>
      (sandboxID ? r.sandboxID === sandboxID : r.chatID === chatID) &&
      r.status === "granted" &&
      (r.expires_at || 0) > Date.now() / 1000,
  );
  const scope = siblings.length
    ? `this chat's workspace, which is also used by ${siblings.map((s) => `“${s}”`).join(", ")}`
    : "this chat's workspace";
  const report = useRef(onState);
  report.current = onState;
  const grantKey = grants.map((g) => g.request_id).join(",");
  useEffect(() => {
    report.current?.({ pending, grants });
  }, [pending?.request_id, grantKey]);
  useEffect(() => {
    let stopped = false;
    const refresh = async () => {
      try {
        const [s, r] = await Promise.all([
          api<{
            configured: boolean;
            connected: boolean;
            can_write: boolean;
            google?: { configured: boolean; connected: boolean };
          }>("sharing/status"),
          api<{ requests: DocumentRequest[] }>("sharing/state"),
        ]);
        if (!stopped) {
          setStatus(s);
          setRequests(r.requests);
        }
      } catch {
        /* Retry transient service failures without disrupting the composer. */
      }
    };
    void refresh();
    const timer = setInterval(refresh, 2000);
    window.addEventListener("warden-refresh-state", refresh);
    return () => {
      stopped = true;
      clearInterval(timer);
      window.removeEventListener("warden-refresh-state", refresh);
    };
  }, []);
  useEffect(() => {
    if (autoOpen && pending && !dismissed.includes(pending.request_id))
      setOpen(true);
  }, [autoOpen, pending?.request_id, dismissed]);
  useEffect(() => {
    setSelected([]);
  }, [pending?.request_id]);
  async function load(next = "") {
    setBusy(true);
    setError("");
    try {
      const r = await api<{ files: File[]; nextPageToken?: string }>(
        "sharing/files" + (next ? "?page=" + encodeURIComponent(next) : ""),
      );
      setFiles((old) => (next ? [...old, ...r.files] : r.files));
      setPage(r.nextPageToken || "");
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  useEffect(() => {
    if (open && status.connected) void load();
  }, [open, status.connected]);
  async function connect() {
    const popup = window.open(
      "about:blank",
      "warden-google",
      "width=600,height=760",
    );
    setError("");
    try {
      const r = await api<{ authorization_url: string }>("sharing/connect", {});
      if (popup) popup.location.href = r.authorization_url;
      else setError("Allow popups for Warden, then try Google sign-in again.");
    } catch (e) {
      popup?.close();
      setError(String(e));
    }
  }
  async function resolve(allow: boolean) {
    if (!pending && !selecting) return;
    setBusy(true);
    setError("");
    try {
      const result = await api<DocumentRequest>(
        pending ? "sharing/resolve" : "sharing/select",
        {
          id: pending?.request_id,
          chatID,
          access,
          allow,
          documents: selected,
          duration,
        },
      );
      setRequests((old) =>
        old.map((r) => (r.request_id === pending?.request_id ? result : r)),
      );
      if (result.status === "failed") {
        setError(
          "Google creation could not be confirmed. Check Google Docs before trying again; Warden will not retry automatically.",
        );
      } else {
        setOpen(false);
        setSelecting(false);
      }
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  const show = () => {
    setError("");
    setOpen(true);
  };
  useImperativeHandle(ref, () => ({
    open: show,
    decline: () => void resolve(false),
  }));
  // providers.google absent: no Google section at all (after the hooks).
  if (status.google && !status.google.configured) return null;
  return (
    <>
      {trigger === null ? null : trigger ? (
        trigger(show)
      ) : (
        <button className="chip" onClick={show}>
          <FileText size={13} />
          <span>
            {grants.length
              ? `${grants.length} document${grants.length === 1 ? "" : "s"}`
              : "Documents"}
          </span>
        </button>
      )}
      {open && (
        <dialog
          ref={dialog}
          onCancel={() => {
            if (pending) setDismissed((old) => [...old, pending.request_id]);
            setOpen(false);
          }}
          className="sharing-dialog"
          role="dialog"
          aria-modal="true"
          aria-labelledby="sharing-title"
        >
          <header>
            <h2 id="sharing-title">
              {pending?.access === "create"
                ? "Create Google document"
                : pending?.access === "write"
                  ? "Allow document editing"
                  : "Share Google documents"}
            </h2>
            <button
              aria-label="Close document sharing"
              onClick={() => {
                if (pending)
                  setDismissed((old) => [...old, pending.request_id]);
                setOpen(false);
              }}
            >
              Close
            </button>
          </header>
          {pending && (
            <>
              <p>{pending.reason}</p>
              <p>
                <strong>
                  {pending.access === "create"
                    ? `Create “${pending.title}” and allow editing`
                    : pending.access === "write"
                      ? "Read and edit selected documents"
                      : "Read only"}
                </strong>
              </p>
            </>
          )}
          <p className="muted">
            Selected documents are shared with {scope}. Access is limited to the
            permission shown below and expires automatically.
          </p>
          {error && (
            <p role="alert" className="error">
              {error}
            </p>
          )}
          {!ready ? (
            <>
              <p>
                {!canConnectGoogle
                  ? "The demo owner needs to reconnect the shared Google account."
                  : status.configured
                    ? needsWrite && status.connected
                      ? "Reconnect Google to enable document creation and editing."
                      : "Sign in to Google to choose documents."
                    : "Configure Warden’s Google OAuth client to enable document sharing."}
              </p>
              {canConnectGoogle && (
                <button
                  disabled={!status.configured}
                  onClick={() => void connect()}
                >
                  Sign in with Google
                </button>
              )}
            </>
          ) : pending || selecting ? (
            <>
              {canConnectGoogle && (
                <button onClick={() => void connect()}>
                  Reconnect Google account
                </button>
              )}
              {pending?.access !== "create" && (
                <>
                  <p className="muted">
                    Showing documents created September 9, 2026 or later.
                  </p>
                  <label>
                    Find a document
                    <input
                      value={query}
                      onChange={(e) => setQuery(e.target.value)}
                      placeholder="Filter loaded files by name"
                    />
                  </label>
                  <div className="sharing-files">
                    {files
                      .filter((f) =>
                        f.name.toLowerCase().includes(query.toLowerCase()),
                      )
                      .map((f) => (
                        <label
                          key={f.id}
                          className={f.blocked ? "blocked" : ""}
                        >
                          <input
                            type="checkbox"
                            checked={selected.includes(f.id)}
                            disabled={f.blocked}
                            onChange={(e) =>
                              setSelected((old) =>
                                e.target.checked
                                  ? [...old, f.id]
                                  : old.filter((id) => id !== f.id),
                              )
                            }
                          />
                          <span>
                            {f.name}
                            {f.blocked && <small> · Unsharable with AI</small>}
                          </span>
                        </label>
                      ))}
                    {!files.length && !busy && (
                      <p>No Google documents found.</p>
                    )}
                  </div>
                  {page && (
                    <button disabled={busy} onClick={() => void load(page)}>
                      Load more files
                    </button>
                  )}
                </>
              )}
              {selecting && (
                <label>
                  Permission
                  <select
                    value={access}
                    onChange={(e) => setAccess(e.target.value)}
                  >
                    <option value="read">Read only</option>
                    <option value="write">Read and edit</option>
                  </select>
                </label>
              )}
              <label>
                Share access for
                <select
                  value={duration}
                  onChange={(e) => setDuration(Number(e.target.value))}
                >
                  <option value={900}>15 minutes</option>
                  <option value={3600}>1 hour</option>
                  <option value={86400}>24 hours</option>
                  <option value={604800}>7 days</option>
                </select>
              </label>
              {pending?.access !== "create" && (
                <p>{selected.length} selected · maximum 20</p>
              )}
            </>
          ) : null}
          {(pending || selecting) && (
            <footer>
              <button
                disabled={busy}
                onClick={() =>
                  pending ? void resolve(false) : setSelecting(false)
                }
              >
                Decline
              </button>
              <button
                className="primary"
                disabled={
                  busy ||
                  !ready ||
                  (pending?.access !== "create" && !selected.length) ||
                  selected.length > 20
                }
                onClick={() => void resolve(true)}
              >
                {busy
                  ? "Working…"
                  : pending?.access === "create"
                    ? "Create document and allow editing"
                    : pending?.access === "write" || access === "write"
                      ? "Allow editing selected documents"
                      : "Share selected documents"}
              </button>
            </footer>
          )}
          {!pending && !selecting && (
            <div>
              <button
                disabled={!chatID || !status.connected}
                onClick={() => setSelecting(true)}
              >
                Share documents with this workspace
              </button>
              {!grants.length && (
                <p>No documents are shared with this workspace.</p>
              )}
              {grants.map((r) => (
                <div key={r.request_id}>
                  {r.documents.map((d) => (
                    <p key={d.id}>
                      <a href={d.url} target="_blank" rel="noreferrer">
                        {d.title}
                      </a>
                    </p>
                  ))}
                  <p>
                    {r.access === "read" ? "Read only" : "Read and edit"} ·
                    Expires{" "}
                    {new Date((r.expires_at || 0) * 1000).toLocaleString()}
                  </p>
                  <button
                    onClick={() =>
                      void api("sharing/revoke", { id: r.request_id })
                        .then(() =>
                          setRequests((old) =>
                            old.filter((x) => x.request_id !== r.request_id),
                          ),
                        )
                        .catch((e) => setError(String(e)))
                    }
                  >
                    Revoke access
                  </button>
                </div>
              ))}
            </div>
          )}
        </dialog>
      )}
    </>
  );
}
