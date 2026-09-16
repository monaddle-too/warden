'use client';
import { ServerExplorer } from '@/components/server-explorer';
import { useDeferredValue, useEffect, useMemo, useRef, useState } from 'react';
import {
  Activity,
  ArrowDown,
  ArrowUp,
  ArrowUpRight,
  Bookmark,
  Braces,
  Check,
  ChevronLeft,
  ChevronRight,
  Copy,
  Database,
  Download,
  Filter,
  HelpCircle,
  Save,
  Search,
  Shield,
  Upload,
  X,
} from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Checkbox } from '@/components/ui/checkbox';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from '@/components/ui/dialog';
import { ImportDialog } from '@/components/import-dialog';
import { SearchHelp } from '@/components/search-help';
import { JsonView } from '@/components/json-view';
import { useExplorerTools } from '@/lib/webmcp';
import { demoEvents } from '@/lib/demo';
import {
  catalog,
  csv,
  emptyFilters,
  filterEvents,
  flatten,
  histogram,
  parseQuery,
  severityNames,
  str,
  sortEvents,
  type EventRecord,
  type Filters,
  type ImportResult,
} from '@/lib/events';

const count = (n: number) => n.toLocaleString('en-US');
const date = (time: number | null) =>
  time === null ? '' : new Date(time).toISOString().slice(0, 10);
const clock = (time: number | null) =>
  time === null ? 'Unknown time' : new Date(time).toISOString().slice(11, 19);
const toggle = <T,>(items: T[], item: T) =>
  items.includes(item) ? items.filter((x) => x !== item) : [...items, item];
function Severity({ event }: { event: EventRecord }) {
  return (
    <span className={`severity sev-${event.severity}`}>
      <i />
      {event.severityName}
    </span>
  );
}
function Facet({
  title,
  values,
  selected,
  onChange,
}: {
  title: string;
  values: [string, number][];
  selected: string[];
  onChange: (value: string) => void;
}) {
  return (
    <section className="facet">
      <h3>
        {title}
        <span>{values.length}</span>
      </h3>
      {values.map(([name, total]) => (
        <label key={name} className="facet-row">
          <Checkbox
            checked={selected.includes(name)}
            onCheckedChange={() => onChange(name)}
          />
          <span title={name}>{name}</span>
          <small>{count(total)}</small>
        </label>
      ))}
    </section>
  );
}
export default function Home() {
  const [local, setLocal] = useState(false);
  if (!local) return <ServerExplorer onLocal={() => setLocal(true)} />;
  return (
    <>
      <button className="server-return" onClick={() => setLocal(false)}>
        ← Stored events & ingestion
      </button>
      <LocalExplorer />
    </>
  );
}
function LocalExplorer() {
  const [events, setEvents] = useState<EventRecord[]>(demoEvents);
  const [filters, setFilters] = useState<Filters>({ ...emptyFilters });
  const [selected, setSelected] = useState<string | null>('demo:4');
  const [bookmarks, setBookmarks] = useState<string[]>([]);
  const [page, setPage] = useState(0);
  const [ascending, setAscending] = useState(false);
  const [fieldSearch, setFieldSearch] = useState('');
  const [importOpen, setImportOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [exportOpen, setExportOpen] = useState(false);
  const [saveOpen, setSaveOpen] = useState(false);
  const [viewName, setViewName] = useState('');
  const [dataset, setDataset] = useState({
    name: 'Production investigation',
    sample: true,
  });
  const [notice, setNotice] = useState('');
  const [exportFormat, setExportFormat] = useState('json');
  const [savedViews, setSavedViews] = useState<
    { name: string; filters: Filters }[]
  >([]);
  const searchInput = useRef<HTMLInputElement>(null);
  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (cancelled) return;
      try {
        const stored = JSON.parse(
          localStorage.getItem('ocsf-explorer-views') || '[]',
        );
        if (Array.isArray(stored))
          setSavedViews(
            stored
              .filter(
                (v) =>
                  typeof v?.name === 'string' &&
                  typeof v.filters?.query === 'string' &&
                  Array.isArray(v.filters?.classes) &&
                  Array.isArray(v.filters?.sources) &&
                  Array.isArray(v.filters?.severities),
              )
              .slice(0, 20)
              .map((v) => ({
                ...v,
                filters: { ...emptyFilters, ...v.filters },
              })),
          );
      } catch {
        /* Invalid or unavailable storage leaves the viewer usable. */
      }
    });
    return () => {
      cancelled = true;
    };
  }, []);
  useEffect(() => {
    if (!notice) return;
    const timer = setTimeout(() => setNotice(''), 6000);
    return () => clearTimeout(timer);
  }, [notice]);
  useEffect(() => {
    const handler = (event: KeyboardEvent) => {
      if (importOpen || helpOpen || exportOpen || saveOpen) return;
      const target = event.target as HTMLElement;
      const editing =
        ['INPUT', 'TEXTAREA', 'SELECT'].includes(target.tagName) ||
        target.isContentEditable;
      if (event.key === '/' && !editing) {
        event.preventDefault();
        searchInput.current?.focus();
      }
      if (event.key === 'Escape' && !editing) setSelected(null);
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [importOpen, helpOpen, exportOpen, saveOpen]);
  function persistViews(views: { name: string; filters: Filters }[]) {
    setSavedViews(views);
    try {
      localStorage.setItem('ocsf-explorer-views', JSON.stringify(views));
    } catch {
      setNotice('Saved for this session. Browser storage is unavailable.');
    }
  }
  function importEvents(result: ImportResult, name: string, append: boolean) {
    const batch = crypto.randomUUID();
    const imported = result.events.map((event, index) => ({
      ...event,
      id: `${batch}:${index}`,
    }));
    setEvents((previous) => (append ? [...previous, ...imported] : imported));
    setDataset({
      name: append ? `${dataset.name} + ${name}` : name,
      sample: false,
    });
    setFilters({ ...emptyFilters });
    setPage(0);
    setSelected(imported[0]?.id || null);
    if (!append) setBookmarks([]);
    setNotice(
      `Imported ${count(result.events.length)} events${result.rejected ? `; skipped ${count(result.rejected)} malformed records` : ''}.`,
    );
  }
  async function copyJson(event: EventRecord) {
    try {
      await navigator.clipboard.writeText(JSON.stringify(event.raw, null, 2));
      setNotice('Original event JSON copied.');
    } catch {
      setNotice(
        'Clipboard access is unavailable. Use the JSON download instead.',
      );
    }
  }

  const deferredQuery = useDeferredValue(filters.query);
  const update = (patch: Partial<Filters>) => {
    setFilters((f) => ({ ...f, ...patch }));
    setPage(0);
  };
  const filtered = useMemo(
    () =>
      sortEvents(
        filterEvents(events, { ...filters, query: deferredQuery }, bookmarks),
        ascending,
      ),
    [events, filters, deferredQuery, bookmarks, ascending],
  );
  useExplorerTools(events, filters, bookmarks, update, setSelected);
  const active = events.find((e) => e.id === selected);
  const chart = useMemo(() => histogram(filtered), [filtered]);
  const maxBin = Math.max(1, ...chart.bins.map((b) => b.count));
  const facets = useMemo(() => {
    const classes = new Map<string, number>();
    const sources = new Map<string, number>();
    for (const e of events) {
      classes.set(e.className, (classes.get(e.className) || 0) + 1);
      sources.set(e.source, (sources.get(e.source) || 0) + 1);
    }
    return {
      classes: [...classes].sort((a, b) => b[1] - a[1]),
      sources: [...sources].sort((a, b) => b[1] - a[1]),
    };
  }, [events]);
  const high = filtered.filter(
    (e) => e.severity >= 4 && e.severity <= 6,
  ).length;
  const currentPage = Math.min(
    page,
    Math.max(0, Math.ceil(filtered.length / 50) - 1),
  );
  const shown = filtered.slice(currentPage * 50, (currentPage + 1) * 50);
  const dirty =
    filters.query ||
    filters.classes.length ||
    filters.sources.length ||
    filters.severities.length ||
    filters.start ||
    filters.end ||
    filters.flagged ||
    filters.bookmarked;
  const queryError = parseQuery(filters.query).error;
  const dateError =
    filters.start &&
    filters.end &&
    Date.parse(`${filters.start}Z`) > Date.parse(`${filters.end}Z`)
      ? 'End time must be on or after the start time.'
      : '';
  const fields = useMemo(
    () =>
      active
        ? flatten(active.raw).filter((f) =>
            `${f.path} ${str(f.value)}`
              .toLowerCase()
              .includes(fieldSearch.toLowerCase()),
          )
        : [],
    [active, fieldSearch],
  );
  return (
    <div className="app-shell">
      <header className="topbar">
        <div className="brand">
          <div className="brand-mark">
            <Braces size={23} />
          </div>
          <strong>
            OCSF <span>Explorer</span>
          </strong>
          <span className="version">LOCAL</span>
        </div>
        <div className="top-actions">
          <span className="privacy">
            <Shield size={14} /> Data stays in your browser
          </span>
          <Button
            variant="outline"
            onClick={() => {
              setEvents(demoEvents());
              update({ ...emptyFilters });
              setSelected('demo:4');
              setDataset({ name: 'Production investigation', sample: true });
              setBookmarks([]);
            }}
          >
            Sample dataset
          </Button>
        </div>
      </header>
      <aside className="sidebar">
        <div className="workspace-label">WORKSPACE</div>
        <button
          className={`nav-item ${!filters.bookmarked && !filters.flagged ? 'active' : ''}`}
          onClick={() => update({ bookmarked: false, flagged: false })}
        >
          <Activity size={17} />
          All events<span>{count(events.length)}</span>
        </button>
        <button
          className={`nav-item ${filters.bookmarked ? 'active' : ''}`}
          onClick={() =>
            update({ bookmarked: !filters.bookmarked, flagged: false })
          }
        >
          <Bookmark size={17} />
          Bookmarks<span>{bookmarks.length}</span>
        </button>
        <button
          className={`nav-item ${filters.flagged ? 'active' : ''}`}
          onClick={() =>
            update({ flagged: !filters.flagged, bookmarked: false })
          }
        >
          <Check size={17} />
          Data quality
          <span>{events.filter((e) => e.issues.length).length}</span>
        </button>
        <div className="filter-heading">
          <Filter size={14} />
          <span>FILTERS</span>
          {!!dirty && (
            <button onClick={() => update({ ...emptyFilters })}>Reset</button>
          )}
        </div>
        <section className="facet">
          <h3>Severity</h3>
          {[
            6,
            5,
            4,
            3,
            2,
            1,
            0,
            99,
            -1,
            ...new Set(
              events
                .map((e) => e.severity)
                .filter((s) => ![6, 5, 4, 3, 2, 1, 0, 99, -1].includes(s)),
            ),
          ].map((severity) => {
            const n = events.filter((e) => e.severity === severity).length;
            return n ? (
              <label className="facet-row" key={severity}>
                <Checkbox
                  checked={filters.severities.includes(severity)}
                  onCheckedChange={() =>
                    update({ severities: toggle(filters.severities, severity) })
                  }
                />
                <span className={`severity sev-${severity}`}>
                  <i />
                  {severityNames[severity] || 'Unspecified'}
                </span>
                <small>{count(n)}</small>
              </label>
            ) : null;
          })}
        </section>
        <Facet
          title="Event class"
          values={facets.classes}
          selected={filters.classes}
          onChange={(value) =>
            update({ classes: toggle(filters.classes, value) })
          }
        />
        <Facet
          title="Source"
          values={facets.sources}
          selected={filters.sources}
          onChange={(value) =>
            update({ sources: toggle(filters.sources, value) })
          }
        />
        <div className="sidebar-footer">
          <span className="status-dot" />
          Offline schema dictionary<span>v{catalog.version}</span>
        </div>
      </aside>
      <main className="main">
        <div className="page-heading">
          <div>
            <div className="breadcrumb">
              Workspace <ChevronRight size={12} /> Event explorer
            </div>
            <h1>
              {filters.bookmarked
                ? 'Bookmarked events'
                : filters.flagged
                  ? 'Data quality'
                  : 'Event explorer'}
            </h1>
            <p>Explore security events through a common language.</p>
          </div>
          <div className="heading-actions">
            <Button
              variant="outline"
              disabled={!filtered.length}
              onClick={() => setExportOpen(true)}
            >
              <Download />
              Export
            </Button>
            <Button onClick={() => setImportOpen(true)}>
              <Upload />
              Import events
            </Button>
          </div>
        </div>
        <div className="dataset-bar">
          <div className="dataset-icon">
            <Database size={16} />
          </div>
          <strong title={dataset.name}>{dataset.name}</strong>
          <span className={dataset.sample ? 'sample-label' : 'local-label'}>
            {dataset.sample ? 'SAMPLE DATA' : 'LOCAL FILE'}
          </span>
          <span className="dataset-note">
            {count(events.length)} {dataset.sample ? 'fictional ' : ''}events ·{' '}
            {facets.sources.length} sources
          </span>
          <span className="dataset-right">
            {dataset.sample ? 'Sep 9, 2026' : 'This tab session'}
            <span> · UTC</span>
          </span>
        </div>
        <div className="search-row">
          <Search size={19} />
          <Input
            ref={searchInput}
            aria-invalid={!!queryError}
            aria-describedby={queryError ? 'query-error' : undefined}
            aria-label="Search events"
            placeholder='Search events or use a field, e.g. severity:high actor:"svc-deploy"'
            value={filters.query}
            onChange={(e) => update({ query: e.target.value })}
          />
          {filters.query ? (
            <Button
              variant="ghost"
              size="icon"
              aria-label="Clear search"
              onClick={() => update({ query: '' })}
            >
              <X />
            </Button>
          ) : (
            <kbd>/</kbd>
          )}
          <Button
            variant="ghost"
            size="icon"
            aria-label="Search syntax help"
            onClick={() => setHelpOpen(true)}
          >
            <HelpCircle />
          </Button>
        </div>
        {queryError && (
          <p id="query-error" className="error-text" role="alert">
            {queryError}
          </p>
        )}
        <div className="quick-views">
          <span>Quick views</span>
          <button
            onClick={() => update({ ...emptyFilters, severities: [4, 5, 6] })}
          >
            High priority
          </button>
          <button
            onClick={() =>
              update({ ...emptyFilters, query: 'class_uid=3002 status_id=2' })
            }
          >
            Authentication failures
          </button>
          {savedViews.map((view) => (
            <div className="saved-view" key={view.name}>
              <button
                onClick={() => {
                  setFilters({ ...view.filters });
                  setPage(0);
                }}
              >
                {view.name}
              </button>
              <button
                aria-label={`Delete saved view ${view.name}`}
                onClick={() =>
                  persistViews(savedViews.filter((v) => v.name !== view.name))
                }
              >
                <X size={11} />
              </button>
            </div>
          ))}
          <button
            className="save-view"
            disabled={savedViews.length >= 20}
            onClick={() => {
              setViewName('');
              setSaveOpen(true);
            }}
          >
            <Save size={12} />
            Save view
          </button>
        </div>
        <div className="time-controls">
          <span>
            <Filter size={13} /> Time range
          </span>
          <input
            aria-label="Start time UTC"
            type="datetime-local"
            step="0.001"
            value={filters.start}
            onChange={(e) => update({ start: e.target.value })}
          />
          <span>to</span>
          <input
            aria-label="End time UTC"
            type="datetime-local"
            step="0.001"
            value={filters.end}
            onChange={(e) => update({ end: e.target.value })}
          />
          <span>UTC</span>
          {(filters.start || filters.end) && (
            <button onClick={() => update({ start: '', end: '' })}>
              Clear
            </button>
          )}
        </div>
        {dateError && (
          <p className="error-text" role="alert">
            {dateError}
          </p>
        )}
        <section className="timeline">
          <div className="timeline-header">
            <div>
              <strong>{count(filtered.length)}</strong>
              <span> matching events</span>
              <span className="timeline-divider" />
              <span className="high-count">
                <i />
                {count(high)} high or above
              </span>
            </div>
            <small>EVENT VOLUME · CLICK A BAR TO FILTER</small>
          </div>
          <div className="chart">
            {chart.bins.map((bin, i) => (
              <button
                key={i}
                title={`${clock(bin.start)} – ${clock(bin.end)} UTC: ${bin.count} events, ${bin.high} high or above`}
                aria-label={`${bin.count} events at ${clock(bin.start)} UTC. Filter this interval.`}
                className="chart-column"
                disabled={!bin.count}
                onClick={() =>
                  update({
                    start: new Date(bin.start).toISOString().slice(0, 23),
                    end: new Date(bin.end).toISOString().slice(0, 23),
                  })
                }
              >
                <span
                  style={{
                    height: `${Math.max(2, (bin.count / maxBin) * 100)}%`,
                  }}
                >
                  <i
                    style={{
                      height: `${bin.count ? (bin.high / bin.count) * 100 : 0}%`,
                    }}
                  />
                </span>
              </button>
            ))}
          </div>
          <div className="chart-labels">
            <span>
              {chart.bins.length ? clock(chart.start) : 'No timed events'}
            </span>
            <span>
              {chart.bins.length ? clock((chart.start + chart.end) / 2) : ''}
            </span>
            <span>{chart.bins.length ? clock(chart.end) : ''} UTC</span>
          </div>
        </section>
        <div className="results-bar">
          <strong>
            Events <span>{count(filtered.length)}</span>
          </strong>
          <span>
            {dirty ? (
              <button onClick={() => update({ ...emptyFilters })}>
                Clear all filters
              </button>
            ) : (
              'All event classes'
            )}
            <span className="muted">
              {' '}
              · {ascending ? 'oldest' : 'newest'} first
            </span>
          </span>
        </div>
        <div className={`investigation ${active ? 'has-inspector' : ''}`}>
          <section className="event-list">
            {/* oxlint-disable jsx-a11y/no-noninteractive-tabindex -- Scroll regions need keyboard focus for arrow-key scrolling. */}
            <section
              className="table-scroll"
              tabIndex={0}
              aria-label="Event results"
            >
              <table>
                <thead>
                  <tr>
                    <th className="bookmark-cell">
                      <Bookmark size={13} />
                    </th>
                    <th>
                      <button
                        className="sort-button"
                        onClick={() => {
                          setAscending((a) => !a);
                          setPage(0);
                        }}
                      >
                        Time (UTC)
                        {ascending ? (
                          <ArrowUp size={13} />
                        ) : (
                          <ArrowDown size={13} />
                        )}
                      </button>
                    </th>
                    <th>Severity</th>
                    <th>Event</th>
                    <th>Actor / source</th>
                    <th className="open-cell">
                      <span className="sr-only">Inspect</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {shown.map((event) => (
                    <tr
                      key={event.id}
                      className={event.id === selected ? 'selected' : ''}
                    >
                      <td>
                        <button
                          className={`bookmark-button ${bookmarks.includes(event.id) ? 'is-saved' : ''}`}
                          aria-label={`${bookmarks.includes(event.id) ? 'Remove bookmark for' : 'Bookmark'} ${event.message}`}
                          onClick={() =>
                            setBookmarks((b) => toggle(b, event.id))
                          }
                        >
                          <Bookmark size={14} />
                        </button>
                      </td>
                      <td className="mono">
                        <span>{clock(event.time)}</span>
                        <small className="event-date">{date(event.time)}</small>
                      </td>
                      <td>
                        <Severity event={event} />
                      </td>
                      <td>
                        <button
                          className="event-title"
                          onClick={() => {
                            setSelected(event.id);
                            setFieldSearch('');
                          }}
                        >
                          {event.message}
                        </button>
                        <div className="event-meta">
                          <span className="class-tag">{event.className}</span>
                          <span>{event.activity}</span>
                          {event.issues.length > 0 && (
                            <span
                              className="quality-dot"
                              title="Basic field issues"
                            >
                              !
                            </span>
                          )}
                        </div>
                      </td>
                      <td>
                        <div className="actor-name">{event.actor}</div>
                        <div className="source-name">{event.source}</div>
                      </td>
                      <td>
                        <button
                          aria-label={`Inspect ${event.message}`}
                          onClick={() => setSelected(event.id)}
                        >
                          <ChevronRight size={16} />
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {!shown.length && (
                <div className="empty-state">
                  <Search size={28} />
                  <h2>No matching events</h2>
                  <p>Try a broader search or remove some filters.</p>
                  <Button
                    variant="outline"
                    onClick={() => update({ ...emptyFilters })}
                  >
                    Clear filters
                  </Button>
                </div>
              )}
            </section>
            {/* oxlint-enable jsx-a11y/no-noninteractive-tabindex */}
            <footer className="pagination">
              <span>
                {filtered.length
                  ? `${currentPage * 50 + 1}–${Math.min((currentPage + 1) * 50, filtered.length)}`
                  : '0'}{' '}
                of {count(filtered.length)}
              </span>
              <div>
                <Button
                  variant="ghost"
                  size="icon"
                  disabled={currentPage === 0}
                  onClick={() => setPage(currentPage - 1)}
                  aria-label="Previous page"
                >
                  <ChevronLeft />
                </Button>
                <span>
                  Page {currentPage + 1} /{' '}
                  {Math.max(1, Math.ceil(filtered.length / 50))}
                </span>
                <Button
                  variant="ghost"
                  size="icon"
                  disabled={(currentPage + 1) * 50 >= filtered.length}
                  onClick={() => setPage(currentPage + 1)}
                  aria-label="Next page"
                >
                  <ChevronRight />
                </Button>
              </div>
            </footer>
          </section>
          {active && (
            <aside className="inspector" aria-label="Event details">
              <div className="inspector-header">
                <span>
                  <span className="status-dot" />
                  EVENT DETAILS
                </span>
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label="Close event details"
                  onClick={() => setSelected(null)}
                >
                  <X />
                </Button>
              </div>
              <div className="inspector-summary">
                {!filtered.some((e) => e.id === active.id) && (
                  <p className="outside-filter">
                    This event is outside the current filters.
                  </p>
                )}
                <div className="inspector-badges">
                  <Severity event={active} />
                  <span className="class-id">{active.classId}</span>
                </div>
                <h2>{active.message}</h2>
                <p>
                  {active.time !== null
                    ? new Date(active.time)
                        .toISOString()
                        .replace('T', ' ')
                        .replace('Z', ' UTC')
                    : 'Timestamp unavailable'}
                </p>
                <div className="summary-actions">
                  <Button
                    variant="outline"
                    onClick={() => setBookmarks((b) => toggle(b, active.id))}
                  >
                    <Bookmark />
                    {bookmarks.includes(active.id) ? 'Bookmarked' : 'Bookmark'}
                  </Button>
                  <Button
                    variant="ghost"
                    onClick={() =>
                      download(
                        'ocsf-event.json',
                        JSON.stringify(active.raw, null, 2),
                      )
                    }
                  >
                    <Download />
                    JSON
                  </Button>
                </div>
              </div>
              <Tabs defaultValue="overview" className="detail-tabs">
                <TabsList variant="line">
                  <TabsTrigger value="overview">Overview</TabsTrigger>
                  <TabsTrigger value="fields">
                    Fields <small>{fields.length}</small>
                  </TabsTrigger>
                  <TabsTrigger value="json">JSON</TabsTrigger>
                </TabsList>
                <TabsContent value="overview">
                  <div className="detail-section">
                    <h3>CLASSIFICATION</h3>
                    <dl>
                      <dt>Event class</dt>
                      <dd>{active.className}</dd>
                      <dt>Category</dt>
                      <dd>{active.category}</dd>
                      <dt>Activity</dt>
                      <dd>{active.activity}</dd>
                      <dt>Status</dt>
                      <dd
                        className={active.status === 'Failure' ? 'failure' : ''}
                      >
                        {active.status}
                      </dd>
                      <dt>Source</dt>
                      <dd>{active.source}</dd>
                      <dt>Schema version</dt>
                      <dd>
                        {str(
                          (active.raw.metadata as Record<string, unknown>)
                            ?.version,
                        ) || 'Unknown'}
                      </dd>
                    </dl>
                  </div>
                  <div className="detail-section">
                    <h3>ENTITIES</h3>
                    <dl>
                      <dt>Actor</dt>
                      <dd>
                        <button
                          onClick={() =>
                            update({
                              query: `actor="${active.actor.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"`,
                            })
                          }
                        >
                          {active.actor}
                          <ArrowUpRight size={12} />
                        </button>
                      </dd>
                      <dt>Target</dt>
                      <dd>{active.target}</dd>
                    </dl>
                  </div>
                  {[
                    'finding_info',
                    'src_endpoint',
                    'dst_endpoint',
                    'process',
                    'api',
                    'query',
                    'traffic',
                  ]
                    .filter((key) => active.raw[key])
                    .map((key) => (
                      <div className="detail-section" key={key}>
                        <h3>{key.replace(/_/g, ' ').toUpperCase()}</h3>
                        <dl>
                          {flatten(active.raw[key])
                            .slice(0, 8)
                            .map((field) => (
                              <div className="context-pair" key={field.path}>
                                <dt>{field.path.replace(/_/g, ' ')}</dt>
                                <dd>
                                  {field.value === null
                                    ? 'null'
                                    : str(field.value)}
                                </dd>
                              </div>
                            ))}
                        </dl>
                      </div>
                    ))}
                  <div className="detail-section">
                    <h3>BASIC FIELD CHECKS</h3>
                    {active.issues.length ? (
                      <ul className="issues-list">
                        {active.issues.map((issue) => (
                          <li key={issue}>{issue}</li>
                        ))}
                      </ul>
                    ) : (
                      <p className="check-result">
                        <Check size={15} />
                        Base fields look consistent
                      </p>
                    )}
                    <p className="detail-note">
                      Checks use the bundled {catalog.version} dictionary. This
                      is not full schema validation.
                    </p>
                  </div>
                </TabsContent>
                <TabsContent value="fields">
                  <div className="field-search">
                    <Input
                      aria-label="Find a field"
                      placeholder="Find a field or value…"
                      value={fieldSearch}
                      onChange={(e) => setFieldSearch(e.target.value)}
                    />
                  </div>
                  <div className="field-list">
                    {fields.slice(0, 1000).map((field) => (
                      <div className="field" key={field.path}>
                        <code
                          title={
                            catalog.classes[String(active.classId)]?.attributes[
                              field.path
                            ]?.description
                          }
                        >
                          {field.path}
                        </code>
                        <span>
                          {field.value === null ? 'null' : str(field.value)}
                        </span>
                        {field.value !== null &&
                          typeof field.value !== 'object' && (
                            <button
                              className="field-pivot"
                              aria-label={`Search for ${field.path}`}
                              onClick={() =>
                                update({
                                  query: `${field.path}=${JSON.stringify(str(field.value))}`,
                                })
                              }
                            >
                              <Search size={12} />
                            </button>
                          )}
                      </div>
                    ))}
                    {fields.length > 1000 && (
                      <p className="detail-note">
                        Showing the first 1,000 fields. Refine your search to
                        find more.
                      </p>
                    )}
                    {!fields.length && (
                      <p className="detail-note">No matching fields.</p>
                    )}
                  </div>
                </TabsContent>
                <TabsContent value="json">
                  <div className="json-toolbar">
                    <span>Original event · unchanged</span>
                    <Button
                      variant="ghost"
                      onClick={() => void copyJson(active)}
                    >
                      <Copy />
                      Copy
                    </Button>
                  </div>
                  <JsonView value={active.raw} />
                </TabsContent>
              </Tabs>
            </aside>
          )}
        </div>
        <div className="bottom-note">
          <Shield size={13} /> Events are processed locally. Nothing is
          uploaded.<span>OCSF Explorer</span>
        </div>
      </main>
      <ImportDialog
        open={importOpen}
        onOpenChange={setImportOpen}
        onImport={importEvents}
        currentCount={events.length}
        isSample={dataset.sample}
      />
      <SearchHelp open={helpOpen} onOpenChange={setHelpOpen} />
      <Dialog open={exportOpen} onOpenChange={setExportOpen}>
        <DialogContent className="export-modal">
          <DialogTitle>Export matching events</DialogTitle>
          <DialogDescription>
            {count(filtered.length)} events in the current filtered and sorted
            result, across all pages.
          </DialogDescription>
          <label className="export-format">
            Format
            <select
              value={exportFormat}
              onChange={(e) => setExportFormat(e.target.value)}
            >
              <option value="json">JSON · original event array</option>
              <option value="ndjson">
                NDJSON · one original event per line
              </option>
              <option value="csv">CSV · visible summary columns</option>
            </select>
          </label>
          <p className="help-note">
            JSON and NDJSON preserve original fields and values. CSV is a
            summary for spreadsheets.
          </p>
          <Button
            onClick={() => {
              const content =
                exportFormat === 'csv'
                  ? csv(filtered)
                  : exportFormat === 'ndjson'
                    ? filtered.map((e) => JSON.stringify(e.raw)).join('\n')
                    : JSON.stringify(
                        filtered.map((e) => e.raw),
                        null,
                        2,
                      );
              download(
                `ocsf-events.${exportFormat}`,
                content,
                exportFormat === 'csv'
                  ? 'text/csv'
                  : exportFormat === 'ndjson'
                    ? 'application/x-ndjson'
                    : 'application/json',
              );
              setExportOpen(false);
              setNotice(`Exported ${count(filtered.length)} events.`);
            }}
          >
            <Download />
            Download {exportFormat.toUpperCase()}
          </Button>
        </DialogContent>
      </Dialog>
      <Dialog open={saveOpen} onOpenChange={setSaveOpen}>
        <DialogContent className="save-modal">
          <DialogTitle>Save this view</DialogTitle>
          <DialogDescription>
            Save the current search and filters in this browser. Event data and
            bookmarks are not saved.
          </DialogDescription>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              if (!viewName.trim()) return;
              persistViews([
                ...savedViews.filter((v) => v.name !== viewName.trim()),
                { name: viewName.trim(), filters: { ...filters } },
              ]);
              setSaveOpen(false);
            }}
          >
            <Input
              aria-label="View name"
              placeholder="e.g. Production auth failures"
              maxLength={50}
              value={viewName}
              onChange={(e) => setViewName(e.target.value)}
            />
            <Button type="submit" disabled={!viewName.trim()}>
              <Save />
              Save view
            </Button>
          </form>
        </DialogContent>
      </Dialog>
      {notice && (
        <output className="toast">
          <Check size={17} />
          {notice}
          <button
            aria-label="Dismiss notification"
            onClick={() => setNotice('')}
          >
            <X size={15} />
          </button>
        </output>
      )}
    </div>
  );
}
function download(name: string, content: string, type = 'application/json') {
  const url = URL.createObjectURL(new Blob([content], { type }));
  const link = document.createElement('a');
  link.href = url;
  link.download = name;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
