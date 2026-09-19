/* Bug reports other installs sent to this Warden's edge
   (docs/bug-reporting-plan.md): the owner's list rows and the stored
   report, plus the pure shaping the admin console's section renders. */

export type BugReportRow = {
  id: string;
  receivedAt: string;
  kind: string;
  component: string;
  version: string;
  os: string;
  arch: string;
  summary: string;
};

export type BugReportLog = { name: string; lines: string[] };

export type BugReport = {
  schema: number;
  id: string;
  kind: string;
  createdAt?: string;
  component: string;
  trigger?: string;
  summary: string;
  description?: string;
  error?: { message?: string; stack?: string; operation?: string };
  warden?: {
    version?: string;
    protocol?: number;
    runtime?: string;
    installed?: { codex?: string; claude?: string; guestArch?: string };
  };
  system?: {
    os?: string;
    arch?: string;
    osVersion?: string;
    cpus?: number;
    memoryMB?: number;
  };
  logs?: BugReportLog[];
  context?: Record<string, string | undefined>;
  receivedAt?: string;
  source?: string;
  [extra: string]: unknown;
};

/* Newest first; the edge lists them so, and pages merged from several
   requests keep the order. */
export function sortReports<T extends { id: string; receivedAt: string }>(
  rows: T[],
): T[] {
  return [...rows].sort(
    (a, b) =>
      b.receivedAt.localeCompare(a.receivedAt) || b.id.localeCompare(a.id),
  );
}

/* Rows of a further page appended to the ones shown, an id never twice. */
export function mergeReports<T extends { id: string; receivedAt: string }>(
  shown: T[],
  page: T[],
): T[] {
  const seen = new Set(shown.map((r) => r.id));
  return sortReports([...shown, ...page.filter((r) => !seen.has(r.id))]);
}

export const kindLabel = (kind: string) =>
  kind === "user" ? "User report" : kind === "error" ? "Error" : kind;

/* "darwin/arm64", or whichever half is known. */
export const platform = (os?: string, arch?: string) =>
  [os, arch].filter(Boolean).join("/") || "—";

/* The installer's version tag without the build noise: v0.1.0-alpha.12
   from v0.1.0-alpha.12-210-g81d0bfc, with the short sha kept aside. */
export function versionParts(version?: string): { tag: string; sha: string } {
  if (!version) return { tag: "—", sha: "" };
  const m = /^(.*?)-\d+-g([0-9a-f]{7,})(-dirty)?$/.exec(version);
  if (!m) return { tag: version, sha: "" };
  return { tag: m[1], sha: m[2] + (m[3] || "") };
}

export const memory = (mb?: number) =>
  !mb
    ? ""
    : mb >= 1024
      ? `${(mb / 1024).toFixed(mb % 1024 ? 1 : 0)} GiB`
      : `${mb} MiB`;

/* The environment table of the detail view: label/value pairs, empty
   values left out so a sparse report shows only what it carries. */
export function environmentRows(r: BugReport): [string, string][] {
  const rows: [string, string | undefined][] = [
    ["Warden", r.warden?.version],
    [
      "Protocol",
      r.warden?.protocol === undefined ? undefined : String(r.warden.protocol),
    ],
    ["Runtime", r.warden?.runtime],
    ["Codex", r.warden?.installed?.codex],
    ["Claude", r.warden?.installed?.claude],
    ["Guest", r.warden?.installed?.guestArch],
    [
      "System",
      [platform(r.system?.os, r.system?.arch), r.system?.osVersion]
        .filter((v) => v && v !== "—")
        .join(" "),
    ],
    ["CPUs", r.system?.cpus === undefined ? undefined : String(r.system.cpus)],
    ["Memory", memory(r.system?.memoryMB)],
    ["Trigger", r.trigger],
    ["Operation", r.error?.operation],
    ["Created", r.createdAt && when(r.createdAt)],
    ["Received", r.receivedAt && when(r.receivedAt)],
    ["Source", r.source && r.source.slice(0, 12)],
  ];
  for (const [key, value] of Object.entries(r.context || {}))
    if (value) rows.push([contextLabel(key), value]);
  return rows.filter((row): row is [string, string] => !!row[1]);
}

const contextLabels: Record<string, string> = {
  chatID: "Chat",
  runID: "Run",
  provider: "Provider",
  model: "Model",
};
export const contextLabel = (key: string) =>
  contextLabels[key] || key.charAt(0).toUpperCase() + key.slice(1);

export const when = (iso: string) => {
  const t = new Date(iso);
  return Number.isNaN(t.getTime()) ? iso : t.toLocaleString();
};

/* The stored report as the edge keeps it, pretty-printed for the raw
   view. */
export const rawJSON = (r: BugReport) => JSON.stringify(r, null, 2);
