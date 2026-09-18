import { useCallback, useEffect, useState } from "react";
import { Bug, RefreshCw, Trash2, X } from "lucide-react";
import { api, apiDelete } from "../api";
import {
  environmentRows,
  kindLabel,
  mergeReports,
  platform,
  rawJSON,
  sortReports,
  versionParts,
  when,
  type BugReport,
  type BugReportRow,
} from "../bugreports";

/* The admin console's Bug reports section (docs/bug-reporting-plan.md):
   what other installs sent to this Warden's edge, newest first, one
   opened at a time with its description, error, logs and environment.
   Rendered only where the edge receives reports: elsewhere the list
   route answers 404 and the section stays out. */
const PAGE = 100;

function Detail({
  id,
  onClose,
  onDeleted,
}: {
  id: string;
  onClose: () => void;
  onDeleted: (id: string) => void;
}) {
  const [report, setReport] = useState<BugReport>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let cancelled = false;
    setReport(undefined);
    setError("");
    api<BugReport>("admin/bug-reports/" + id)
      .then((r) => !cancelled && setReport(r))
      .catch((e) => !cancelled && setError(String(e)));
    return () => {
      cancelled = true;
    };
  }, [id]);
  async function remove() {
    if (!window.confirm("Delete this bug report? It cannot be recovered."))
      return;
    setBusy(true);
    try {
      await apiDelete("admin/bug-reports/" + id);
      onDeleted(id);
    } catch (e) {
      setError(String(e));
      setBusy(false);
    }
  }
  const environment = report ? environmentRows(report) : [];
  return (
    <div className="bug-report" role="region" aria-label="Bug report">
      <header>
        <strong>{report?.summary || id}</strong>
        {report && (
          <small className="muted">
            {kindLabel(report.kind)} · {report.component}
            {report.receivedAt && <> · received {when(report.receivedAt)}</>}
          </small>
        )}
        <button className="danger" disabled={busy || !report} onClick={remove}>
          <Trash2 size={14} />
          Delete
        </button>
        <button className="ghost" onClick={onClose} aria-label="Close report">
          <X size={14} />
        </button>
      </header>
      {error && (
        <p role="alert" className="error">
          {error}
        </p>
      )}
      {!report && !error && <p className="muted">Loading…</p>}
      {report && (
        <>
          {report.description && (
            <>
              <h4>Description</h4>
              <p className="bug-description">{report.description}</p>
            </>
          )}
          {report.error && (report.error.message || report.error.stack) && (
            <>
              <h4>Error</h4>
              {report.error.message && (
                <p className="bug-error-message">{report.error.message}</p>
              )}
              {report.error.stack && <pre>{report.error.stack}</pre>}
            </>
          )}
          {report.logs?.map((log) => (
            <div key={log.name}>
              <h4>
                <code>{log.name}</code>
                <small className="muted">
                  {" "}
                  · {log.lines.length} line{log.lines.length === 1 ? "" : "s"}
                </small>
              </h4>
              <pre>{log.lines.length ? log.lines.join("\n") : "(empty)"}</pre>
            </div>
          ))}
          {environment.length > 0 && (
            <>
              <h4>Environment</h4>
              <table className="cluster-table bug-environment">
                <tbody>
                  {environment.map(([label, value]) => (
                    <tr key={label}>
                      <th scope="row">{label}</th>
                      <td>{value}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </>
          )}
          <details className="bug-raw">
            <summary>Raw report</summary>
            <pre>{rawJSON(report)}</pre>
          </details>
        </>
      )}
    </div>
  );
}

export function BugReports() {
  const [rows, setRows] = useState<BugReportRow[]>();
  const [available, setAvailable] = useState(true);
  const [more, setMore] = useState(false);
  const [open, setOpen] = useState<string>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const load = useCallback(async (before?: string) => {
    setBusy(true);
    try {
      const page = await api<BugReportRow[]>(
        `admin/bug-reports?limit=${PAGE}` +
          (before ? "&before=" + encodeURIComponent(before) : ""),
      );
      setRows((old) =>
        before && old ? mergeReports(old, page) : sortReports(page),
      );
      setMore(page.length === PAGE);
      setError("");
    } catch (e) {
      // 404: this edge does not receive reports.
      if (before || !/not found/i.test(String(e))) setError(String(e));
      else setAvailable(false);
    } finally {
      setBusy(false);
    }
  }, []);
  useEffect(() => {
    void load();
  }, [load]);
  // A refresh that no longer lists the open report closes it.
  useEffect(() => {
    if (open && rows && !rows.some((r) => r.id === open)) setOpen(undefined);
  }, [rows, open]);
  if (!available) return null;
  const last = rows?.[rows.length - 1];
  return (
    <section aria-labelledby="admin-bugs" className="bug-reports">
      <h2 id="admin-bugs">
        <Bug size={16} />
        Bug reports
        <button
          className="ghost"
          disabled={busy}
          onClick={() => void load()}
          aria-label="Refresh bug reports"
        >
          <RefreshCw size={14} />
        </button>
      </h2>
      <p className="muted">
        Reports other Warden installs sent to this edge, each reviewed by the
        person who sent it. Never chat content: an error, its stack, the log
        tail and the environment.
      </p>
      {error && (
        <p role="alert" className="error">
          {error}
        </p>
      )}
      {rows && !rows.length && (
        <p className="muted">No bug reports received.</p>
      )}
      {rows && rows.length > 0 && (
        <table className="cluster-table bug-table">
          <thead>
            <tr>
              <th>Received</th>
              <th>Kind</th>
              <th>Component</th>
              <th>Version</th>
              <th>Platform</th>
              <th>Summary</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => {
              const v = versionParts(r.version);
              return (
                <tr
                  key={r.id}
                  className={open === r.id ? "selected" : ""}
                  onClick={() => setOpen(open === r.id ? undefined : r.id)}
                >
                  <td className="bug-when">{when(r.receivedAt)}</td>
                  <td>
                    <span className={"bug-kind " + r.kind}>
                      {kindLabel(r.kind)}
                    </span>
                  </td>
                  <td>{r.component}</td>
                  <td title={r.version}>
                    {v.tag}
                    {v.sha && <small className="muted"> · {v.sha}</small>}
                  </td>
                  <td className="bug-platform">{platform(r.os, r.arch)}</td>
                  <td className="bug-summary">{r.summary}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
      {more && last && (
        <div className="admin-actions">
          <button disabled={busy} onClick={() => void load(last.id)}>
            Load older reports
          </button>
        </div>
      )}
      {open && (
        <Detail
          id={open}
          onClose={() => setOpen(undefined)}
          onDeleted={(id) => {
            setOpen(undefined);
            setRows((old) => old?.filter((r) => r.id !== id));
          }}
        />
      )}
    </section>
  );
}
