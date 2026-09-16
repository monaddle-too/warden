'use client';
import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Braces,
  Database,
  RefreshCw,
  Upload,
  LogOut,
  Download,
  ArrowLeft,
  ArrowRight,
} from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Tabs, TabsList, TabsTrigger, TabsContent } from '@/components/ui/tabs';
import {
  Dialog,
  DialogContent,
  DialogTitle,
  DialogDescription,
} from '@/components/ui/dialog';
import {
  GoogleLogin,
  type AuthState,
  type BrowserSession,
} from '@/components/google-login';
import { JsonView } from '@/components/json-view';
import { catalog, normalize, severityNames } from '@/lib/events';

type StoredEvent = {
  id: string;
  batch_id: string;
  raw: string;
  time: number;
  received_ms: number;
  source: string;
  stream: string;
  class_uid: number;
  severity_id: number;
};
type SearchResult = {
  events: StoredEvent[];
  total: number;
  next_cursor?: string;
};
type Batch = {
  id: string;
  created_ms: number;
  state: string;
  accepted: number;
  rejected: number;
  attempts: number;
  error?: string;
  rejections?: { record: number; reason: string; raw_base64?: string }[];
};
type Status = {
  role: string;
  storage_ready: boolean;
  last_error?: string;
  retention_days: number;
  queue: Record<string, number>;
};
const date = (n: number) =>
  new Date(n).toISOString().replace('T', ' ').replace('Z', ' UTC');
function save(name: string, value: string) {
  const url = URL.createObjectURL(
    new Blob([value], { type: 'application/json' }),
  );
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

export function ServerExplorer({ onLocal }: { onLocal: () => void }) {
  const [auth, setAuth] = useState<AuthState | null>(null);
  const [error, setError] = useState('');
  const [attempt, setAttempt] = useState(0);
  const onSession = useCallback(
    (session: BrowserSession) => setAuth({ enabled: true, ...session }),
    [],
  );
  const onSignOut = useCallback(() => {
    setAuth(null);
    setAttempt((value) => value + 1);
  }, []);
  useEffect(() => {
    const controller = new AbortController();
    void fetch('/auth/session', {
      credentials: 'same-origin',
      cache: 'no-store',
      signal: controller.signal,
    })
      .then(async (response) => {
        if (!response.ok)
          throw new Error('Could not load sign-in. Please try again.');
        return response.json() as Promise<AuthState>;
      })
      .then((state) => {
        if (!controller.signal.aborted) {
          setAuth(state);
          setError('');
        }
      })
      .catch((e) => {
        if (!controller.signal.aborted) setError(String(e));
      });
    return () => controller.abort();
  }, [attempt]);
  if (auth?.user && auth.csrf)
    return (
      <AuthenticatedExplorer
        session={auth as BrowserSession}
        onLocal={onLocal}
        onSignOut={onSignOut}
      />
    );
  return (
    <div className="server-shell">
      <header className="topbar">
        <div className="brand">
          <div className="brand-mark">
            <Braces size={23} />
          </div>
          <strong>
            OCSF <span>Explorer</span>
          </strong>
          <span className="version">SERVER</span>
        </div>
        <Button variant="outline" onClick={onLocal}>
          Local files
        </Button>
      </header>
      <main className="server-login">
        <Database size={32} />
        <h1>Sign in to OCSF Explorer</h1>
        <p>Use your approved Google account to investigate stored events.</p>
        {!auth && !error && <output>Loading sign-in…</output>}
        {auth?.enabled && (
          <GoogleLogin
            key={attempt}
            auth={auth}
            onSession={onSession}
            onRetry={onSignOut}
          />
        )}
        {auth && !auth.enabled && (
          <output>
            Google sign-in has not been configured on this server.
          </output>
        )}
        {error && (
          <div role="alert">
            <p className="server-error">{error}</p>
            <Button onClick={onSignOut}>Try again</Button>
          </div>
        )}
        <p className="server-muted">
          Access is limited to approved accounts. For offline investigation,
          choose Local files.
        </p>
      </main>
    </div>
  );
}

function AuthenticatedExplorer({
  onLocal,
  onSignOut,
  session,
}: {
  onLocal: () => void;
  onSignOut: () => void;
  session: BrowserSession;
}) {
  const [status, setStatus] = useState<Status | null>(null);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState(true);
  const [result, setResult] = useState<SearchResult>({ events: [], total: 0 });
  const [selected, setSelected] = useState<StoredEvent | null>(null);
  const [query, setQuery] = useState('');
  const [source, setSource] = useState('');
  const [severity, setSeverity] = useState('');
  const [classID, setClassID] = useState('');
  const [start, setStart] = useState('');
  const [end, setEnd] = useState('');
  const [field, setField] = useState('');
  const [value, setValue] = useState('');
  const [params, setParams] = useState('');
  const [cursor, setCursor] = useState('');
  const [history, setHistory] = useState<string[]>([]);
  const [batches, setBatches] = useState<Batch[]>([]);
  const [batch, setBatch] = useState<Batch | null>(null);
  const [tab, setTab] = useState('events');
  const [file, setFile] = useState<File | null>(null);
  const [stream, setStream] = useState('manual');
  const [uploading, setUploading] = useState(false);
  const uploadKey = useRef(crypto.randomUUID());
  const [receipt, setReceipt] = useState<Batch | null>(null);
  const request = useCallback(
    async <T,>(path: string, init: RequestInit = {}) => {
      const headers = new Headers(init.headers);
      headers.set('X-SIEM-CSRF', session.csrf);
      const response = await fetch(path, {
        ...init,
        credentials: 'same-origin',
        headers,
        signal: init.signal || AbortSignal.timeout(30000),
      });
      const data: unknown = await response
        .json()
        .catch(() => ({ error: `HTTP ${response.status}` }));
      if (response.status === 401) onSignOut();
      if (!response.ok)
        throw new Error(
          (data as { error?: string }).error || `HTTP ${response.status}`,
        );
      return data as T;
    },
    [session.csrf, onSignOut],
  );
  useEffect(() => {
    let cancelled = false;
    async function refresh() {
      try {
        const s = await request<Status>('/api/v1/status');
        if (!cancelled) setStatus(s);
      } catch (e) {
        if (!cancelled) setError(String(e));
      }
    }
    void refresh();
    const timer = setInterval(refresh, 15000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [request]);
  useEffect(() => {
    if (tab !== 'events') return;
    const controller = new AbortController();
    // This effect owns an external request lifecycle, including its loading state.
    // oxlint-disable-next-line react/react-compiler
    setBusy(true);
    setError('');
    setSelected(null);
    request<SearchResult>(
      `/api/v1/events?${params}&limit=100&cursor=${encodeURIComponent(cursor)}`,
      { signal: controller.signal },
    )
      .then(setResult)
      .catch((e) => {
        if (!controller.signal.aborted) {
          setError(String(e));
          setResult({ events: [], total: 0 });
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setBusy(false);
      });
    return () => controller.abort();
  }, [params, cursor, tab, request]);
  const refreshBatches = useCallback(async () => {
    try {
      setBatches(
        (await request<{ batches: Batch[] }>('/api/v1/batches')).batches,
      );
    } catch (e) {
      setError(String(e));
    }
  }, [request]);
  // Start and stop the external batch polling subscription when the tab changes.
  useEffect(() => {
    if (tab === 'batches') {
      // oxlint-disable-next-line react/react-compiler
      void refreshBatches();
      const timer = setInterval(refreshBatches, 5000);
      return () => clearInterval(timer);
    }
  }, [tab, refreshBatches]);
  function search() {
    const p = new URLSearchParams();
    for (const [key, val] of Object.entries({
      q: query,
      source,
      severity_id: severity,
      class_uid: classID,
      field,
      value,
    })) {
      if (val) p.set(key, val);
    }
    if (start) p.set('start', String(Date.parse(`${start}Z`)));
    if (end) p.set('end', String(Date.parse(`${end}Z`)));
    p.set('_refresh', String(Date.now()));
    setParams(p.toString());
    setCursor('');
    setHistory([]);
  }
  function showLatest() {
    setQuery('');
    setSource('');
    setSeverity('');
    setClassID('');
    setStart('');
    setEnd('');
    setField('');
    setValue('');
    setParams(`_refresh=${Date.now()}`);
    setCursor('');
    setHistory([]);
  }
  async function upload() {
    if (!file) return;
    setUploading(true);
    setError('');
    try {
      if (file.size > 10 * 1024 * 1024)
        throw new Error(
          'Files must be at most 10 MiB and 2,000 records. Split larger files into batches.',
        );
      const text = await file.text();
      const contentType = /\.ndjson$|\.jsonl$/i.test(file.name)
        ? 'application/x-ndjson'
        : 'application/json';
      const r = await request<Batch>('/api/v1/events', {
        method: 'POST',
        headers: {
          'Content-Type': contentType,
          'Idempotency-Key': uploadKey.current,
          'X-OCSF-Source': stream,
        },
        body: text,
      });
      setReceipt(r);
      setNotice(
        `Batch saved durably: ${r.accepted} accepted, ${r.rejected} quarantined. Indexing happens in the background.`,
      );
      uploadKey.current = crypto.randomUUID();
      setFile(null);
      await refreshBatches();
    } catch (e) {
      setError(String(e));
    } finally {
      setUploading(false);
    }
  }
  const active = selected
    ? normalize(JSON.parse(selected.raw), selected.id)
    : null;
  return (
    <div className="server-shell">
      <header className="topbar">
        <div className="brand">
          <div className="brand-mark">
            <Braces size={23} />
          </div>
          <strong>
            OCSF <span>Explorer</span>
          </strong>
          <span className="version">SERVER</span>
        </div>
        <div className="top-actions">
          <Button variant="outline" onClick={onLocal}>
            Local files
          </Button>
          <span className="server-muted">{session.user.email}</span>
          <Button
            variant="outline"
            onClick={async () => {
              try {
                await request('/auth/logout', { method: 'POST' });
                window.google?.accounts.id.disableAutoSelect();
                onSignOut();
              } catch (e) {
                setError(String(e));
              }
            }}
          >
            <LogOut />
            Sign out
          </Button>
        </div>
      </header>
      <main className="server-main">
        <div className="server-heading">
          <div>
            <h1>Event pipeline</h1>
            <p>
              Durable ingestion and investigation · all times UTC ·{' '}
              {status?.retention_days || 30}-day event retention
            </p>
          </div>
          <span
            className={`server-health ${status?.storage_ready ? 'healthy' : 'degraded'}`}
          >
            {status?.storage_ready
              ? 'ClickHouse connected'
              : 'Storage unavailable · ingestion can queue'}
          </span>
        </div>
        <div className="server-metrics">
          {[
            ['queued', 'Queued batches'],
            ['dead_letter', 'Delivery failures'],
            ['rejected', 'Quarantined records'],
            ['indexed', 'Indexed batches'],
          ].map(([key, label]) => (
            <div key={key}>
              <span>{label}</span>
              <strong>{(status?.queue[key] || 0).toLocaleString()}</strong>
            </div>
          ))}
          <div>
            <span>Queue and receipts · 7-day window</span>
            <strong>
              {((status?.queue.bytes || 0) / 1048576).toFixed(1)} /{' '}
              {((status?.queue.capacity_bytes || 0) / 1048576).toFixed(0)} MiB
            </strong>
          </div>
        </div>
        {status?.last_error && (
          <p className="server-error">
            {status.last_error}. Accepted batches remain on disk while delivery
            retries.
          </p>
        )}
        {error && (
          <p role="alert" className="server-error">
            {error}
          </p>
        )}
        {notice && (
          <output className="server-notice">
            {notice}
            <button onClick={() => setNotice('')} aria-label="Dismiss notice">
              ×
            </button>
          </output>
        )}
        <Tabs value={tab} onValueChange={(v) => setTab(String(v))}>
          <TabsList>
            <TabsTrigger value="events">Stored events</TabsTrigger>
            {status?.role === 'admin' && (
              <TabsTrigger value="batches">Ingestion & batches</TabsTrigger>
            )}
          </TabsList>
          <TabsContent value="events">
            <form
              className="server-filters"
              onSubmit={(e) => {
                e.preventDefault();
                search();
              }}
            >
              <label htmlFor="server-field-2" className="server-wide">
                Search original event JSON
                <Input
                  id="server-field-2"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder="Text or phrase (case-insensitive)"
                />
              </label>
              <label htmlFor="server-field-3">
                Severity
                <select
                  id="server-field-3"
                  value={severity}
                  onChange={(e) => setSeverity(e.target.value)}
                >
                  <option value="">All severities</option>
                  {Object.entries(severityNames).map(([id, name]) => (
                    <option key={id} value={id}>
                      {name}
                    </option>
                  ))}
                </select>
              </label>
              <label htmlFor="server-field-4">
                Event class
                <select
                  id="server-field-4"
                  value={classID}
                  onChange={(e) => setClassID(e.target.value)}
                >
                  <option value="">All classes</option>
                  {Object.entries(catalog.classes).map(([id, c]) => (
                    <option key={id} value={id}>
                      {c.caption}
                    </option>
                  ))}
                </select>
              </label>
              <label htmlFor="server-field-5">
                Product / source
                <Input
                  id="server-field-5"
                  value={source}
                  onChange={(e) => setSource(e.target.value)}
                  placeholder="Exact product name"
                />
              </label>
              <label htmlFor="server-field-6">
                From (UTC)
                <Input
                  id="server-field-6"
                  type="datetime-local"
                  value={start}
                  onChange={(e) => setStart(e.target.value)}
                />
              </label>
              <label htmlFor="server-field-7">
                Through (UTC)
                <Input
                  id="server-field-7"
                  type="datetime-local"
                  value={end}
                  onChange={(e) => setEnd(e.target.value)}
                />
              </label>
              <label htmlFor="server-field-8">
                Exact field path
                <Input
                  id="server-field-8"
                  value={field}
                  onChange={(e) => setField(e.target.value)}
                  placeholder="src_endpoint.ip"
                />
              </label>
              <label htmlFor="server-field-9">
                Field value
                <Input
                  id="server-field-9"
                  value={value}
                  onChange={(e) => setValue(e.target.value)}
                  placeholder="10.0.0.1"
                />
              </label>
              <Button type="submit" disabled={busy}>
                <RefreshCw />
                {busy ? 'Searching…' : 'Search / refresh'}
              </Button>
              <Button
                type="button"
                variant="outline"
                disabled={busy}
                onClick={showLatest}
              >
                Show latest events
              </Button>
            </form>
            <p className="server-muted">
              Newest events appear first. Leave the dates blank to search all
              retained events. Text search is literal; use the field controls
              for exact matches. Refresh to include newly indexed events.
            </p>
            <div className="server-results">
              <div className="server-results-heading">
                <strong aria-live="polite">
                  {busy
                    ? 'Loading events…'
                    : `${result.total.toLocaleString()} matching events`}
                </strong>
                <Button
                  variant="outline"
                  disabled={!result.events.length}
                  onClick={() =>
                    save(
                      'ocsf-page.ndjson',
                      result.events.map((e) => e.raw).join('\n'),
                    )
                  }
                >
                  <Download />
                  Export this page
                </Button>
              </div>
              <div className="server-table-wrap">
                <table className="server-table">
                  <thead>
                    <tr>
                      <th>Time (UTC)</th>
                      <th>Severity</th>
                      <th>Event</th>
                      <th>Source</th>
                      <th>Stream</th>
                    </tr>
                  </thead>
                  <tbody>
                    {result.events.map((event) => {
                      const n = normalize(JSON.parse(event.raw), event.id);
                      return (
                        <tr key={event.id}>
                          <td>{date(event.time)}</td>
                          <td>
                            <span
                              className={`severity sev-${event.severity_id}`}
                            >
                              {n.severityName}
                            </span>
                          </td>
                          <td>
                            <button onClick={() => setSelected(event)}>
                              {(n.message || n.className).slice(0, 240)}
                              {(n.message || n.className).length > 240
                                ? '…'
                                : ''}
                            </button>
                            <small>{n.className}</small>
                          </td>
                          <td>{event.source}</td>
                          <td>{event.stream}</td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
              {!busy && !result.events.length && (
                <div className="server-empty">
                  <Database />
                  <h3>No matching events</h3>
                  <p>
                    Ingest OCSF events, wait for indexing, or broaden the time
                    range and filters.
                  </p>
                  <Button variant="outline" onClick={showLatest}>
                    Show latest events
                  </Button>
                </div>
              )}
              <div className="server-pagination">
                <Button
                  variant="outline"
                  disabled={!history.length || busy}
                  onClick={() => {
                    setCursor(history[history.length - 1]);
                    setHistory(history.slice(0, -1));
                  }}
                >
                  <ArrowLeft />
                  Previous
                </Button>
                <span>
                  Page {history.length + 1} · {result.events.length} events
                </span>
                <Button
                  variant="outline"
                  disabled={!result.next_cursor || busy}
                  onClick={() => {
                    setHistory([...history, cursor]);
                    setCursor(result.next_cursor || '');
                  }}
                >
                  Next
                  <ArrowRight />
                </Button>
              </div>
            </div>
          </TabsContent>
          {status?.role === 'admin' && (
            <TabsContent value="batches">
              <section className="server-upload">
                <div>
                  <h2>Ingest an OCSF batch</h2>
                  <p>
                    JSON event, array, envelope, or NDJSON file. Up to 10 MiB /
                    2,000 records; 256 KiB per record. Invalid records are
                    retained for review.
                  </p>
                </div>
                <label htmlFor="server-field-10">
                  Stream name
                  <Input
                    id="server-field-10"
                    value={stream}
                    disabled={uploading}
                    onChange={(e) => {
                      setStream(e.target.value);
                      uploadKey.current = crypto.randomUUID();
                    }}
                  />
                </label>
                <label htmlFor="server-field-11">
                  Event file {file && `· ${file.name}`}
                  <Input
                    id="server-field-11"
                    key={file?.name || 'empty'}
                    type="file"
                    accept=".json,.jsonl,.ndjson"
                    disabled={uploading}
                    onChange={(e) => {
                      setFile(e.target.files?.[0] || null);
                      uploadKey.current = crypto.randomUUID();
                      setReceipt(null);
                    }}
                  />
                </label>
                <Button disabled={!file || uploading} onClick={upload}>
                  <Upload />
                  {uploading ? 'Saving batch…' : 'Ingest file'}
                </Button>
                <p className="server-muted">
                  A retry after a network error reuses the same idempotency key.
                  File contents leave your browser only when you press Ingest
                  file.
                </p>
                {receipt && (
                  <p>
                    Receipt <code>{receipt.id}</code> · {receipt.accepted}{' '}
                    accepted · {receipt.rejected} quarantined
                  </p>
                )}
              </section>
              <div className="server-results">
                <div className="server-results-heading">
                  <h2>Recent batches</h2>
                  <Button variant="outline" onClick={refreshBatches}>
                    <RefreshCw />
                    Refresh
                  </Button>
                </div>
                <div className="server-table-wrap">
                  <table className="server-table">
                    <thead>
                      <tr>
                        <th>Received</th>
                        <th>Batch</th>
                        <th>State</th>
                        <th>Accepted</th>
                        <th>Quarantined</th>
                        <th>Attempts</th>
                      </tr>
                    </thead>
                    <tbody>
                      {batches.map((b) => (
                        <tr key={b.id}>
                          <td>{date(b.created_ms)}</td>
                          <td>
                            <button
                              onClick={async () => {
                                try {
                                  setBatch(
                                    await request<Batch>(
                                      `/api/v1/batches/${b.id}`,
                                    ),
                                  );
                                } catch (e) {
                                  setError(String(e));
                                }
                              }}
                            >
                              {b.id.slice(-12)}
                            </button>
                          </td>
                          <td>{b.state.replace('_', ' ')}</td>
                          <td>{b.accepted}</td>
                          <td>{b.rejected}</td>
                          <td>{b.attempts}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                {!batches.length && (
                  <p className="server-empty">No batches received yet.</p>
                )}
                <p className="server-muted">
                  Latest 100 receipts. Completed receipts and quarantined
                  records expire after 7 days. Pending deliveries and dead
                  letters remain until resolved.
                </p>
              </div>
            </TabsContent>
          )}
        </Tabs>
      </main>
      <Dialog
        open={!!selected}
        onOpenChange={(open) => {
          if (!open) setSelected(null);
        }}
      >
        <DialogContent className="server-inspector">
          <DialogTitle>
            {active?.message?.slice(0, 240) || 'Event details'}
          </DialogTitle>
          <DialogDescription>
            {selected &&
              `${date(selected.time)} · ${selected.source} · stream ${selected.stream}`}
          </DialogDescription>
          {selected && (
            <>
              <div className="server-results-heading">
                <span>Batch {selected.batch_id.slice(-12)}</span>
                <Button
                  variant="outline"
                  onClick={() =>
                    save(`event-${selected.id}.json`, selected.raw)
                  }
                >
                  <Download />
                  Original JSON
                </Button>
              </div>
              <div className="server-json">
                <JsonView value={JSON.parse(selected.raw)} />
              </div>
            </>
          )}
        </DialogContent>
      </Dialog>
      <Dialog
        open={!!batch}
        onOpenChange={(open) => {
          if (!open) setBatch(null);
        }}
      >
        <DialogContent className="server-inspector">
          <DialogTitle>Batch {batch?.id.slice(-12)}</DialogTitle>
          <DialogDescription>
            {batch?.state} · {batch?.accepted} accepted · {batch?.rejected}{' '}
            quarantined
          </DialogDescription>
          {batch?.error && <p className="server-error">{batch.error}</p>}
          <div className="server-results-heading">
            <Button
              variant="outline"
              onClick={() =>
                save(`batch-${batch?.id}.json`, JSON.stringify(batch, null, 2))
              }
            >
              <Download />
              Download receipt & quarantine
            </Button>
            {batch?.state === 'dead_letter' && (
              <Button
                onClick={async () => {
                  try {
                    await request(`/api/v1/batches/${batch.id}/replay`, {
                      method: 'POST',
                    });
                    setBatch(null);
                    setNotice(
                      'Batch queued for replay with the same event IDs.',
                    );
                    void refreshBatches();
                  } catch (e) {
                    setError(String(e));
                  }
                }}
              >
                Replay delivery
              </Button>
            )}
          </div>
          <p className="server-muted">
            Showing up to 100 quarantined records. Download the receipt for all
            records.
          </p>
          <div className="server-json">
            {batch?.rejections?.slice(0, 100).map((r) => (
              <div key={r.record}>
                <strong>Record {r.record}</strong>
                <p>{r.reason}</p>
                {r.raw_base64 && (
                  <pre>
                    {new TextDecoder().decode(
                      Uint8Array.from(atob(r.raw_base64), (c) =>
                        c.charCodeAt(0),
                      ),
                    )}
                  </pre>
                )}
              </div>
            ))}
            {!batch?.rejections?.length && (
              <p>No quarantined records in this batch.</p>
            )}
          </div>
        </DialogContent>
      </Dialog>
    </div>
  );
}
