import dictionary from './catalog.json' with { type: 'json' };
export type JsonObject = Record<string, unknown>;
type Attribute = {
  caption?: string;
  description?: string;
  type?: string;
  group?: string;
  requirement?: string;
  profile?: string;
  enum?: Record<string, string>;
};
export type ClassDefinition = {
  name: string;
  caption: string;
  category: string;
  category_uid: number;
  attributes: Record<string, Attribute>;
};
export const catalog = dictionary as unknown as {
  version: string;
  source: string;
  classes: Record<string, ClassDefinition>;
};
export const severityNames: Record<number, string> = {
  0: 'Unknown',
  1: 'Informational',
  2: 'Low',
  3: 'Medium',
  4: 'High',
  5: 'Critical',
  6: 'Fatal',
  99: 'Other',
};
export const MAX_BYTES = 25 * 1024 * 1024;
export const MAX_EVENTS = 50_000;
export type EventRecord = {
  id: string;
  raw: JsonObject;
  time: number | null;
  classId: number | null;
  className: string;
  category: string;
  severity: number;
  severityName: string;
  activity: string;
  status: string;
  source: string;
  actor: string;
  target: string;
  message: string;
  issues: string[];
  search: string;
};
export type ImportProblem = { location: string; message: string };
export type ImportResult = {
  events: EventRecord[];
  rejected: number;
  problems: ImportProblem[];
  total: number;
};
export const object = (value: unknown): value is JsonObject =>
  value !== null && typeof value === 'object' && !Array.isArray(value);
export function get(value: unknown, path: string): unknown {
  let current = value;
  for (const segment of path.split('.')) {
    if (
      current === null ||
      typeof current !== 'object' ||
      !Object.hasOwn(current, segment)
    )
      return undefined;
    current = (current as JsonObject)[segment];
  }
  return current;
}
export const str = (value: unknown): string =>
  typeof value === 'string'
    ? value
    : typeof value === 'number' || typeof value === 'boolean'
      ? String(value)
      : '';
export const pick = (raw: JsonObject, ...paths: string[]) =>
  paths.map((path) => str(get(raw, path))).find(Boolean) || '';
export function eventTime(raw: JsonObject): number | null {
  if (
    typeof raw.time === 'number' &&
    Number.isFinite(raw.time) &&
    Math.abs(raw.time) <= 8.64e15
  )
    return raw.time;
  if (typeof raw.time_dt === 'string') {
    const time = Date.parse(raw.time_dt);
    if (Number.isFinite(time)) return time;
  }
  return null;
}
export function normalize(raw: JsonObject, id: string): EventRecord {
  const classId =
    typeof raw.class_uid === 'number' && Number.isInteger(raw.class_uid)
      ? raw.class_uid
      : null;
  const def = catalog.classes[String(classId)];
  const severity = typeof raw.severity_id === 'number' ? raw.severity_id : -1;
  const severityName =
    (severity === 99 ? str(raw.severity) : '') ||
    severityNames[severity] ||
    'Unspecified';
  const className =
    str(raw.class_name) ||
    def?.caption ||
    (classId === null ? 'Unclassified' : `Class ${classId}`);
  const category = str(raw.category_name) || def?.category || 'Uncategorized';
  const activity =
    str(raw.activity_name) ||
    def?.attributes.activity_id?.enum?.[str(raw.activity_id)] ||
    `Activity ${str(raw.activity_id) || '?'}`;
  const status =
    str(raw.status) ||
    (
      { 0: 'Unknown', 1: 'Success', 2: 'Failure', 99: 'Other' } as Record<
        string,
        string
      >
    )[str(raw.status_id)] ||
    'Unspecified';
  const time = eventTime(raw);
  const issues: string[] = [];
  for (const key of [
    'class_uid',
    'category_uid',
    'activity_id',
    'type_uid',
    'severity_id',
  ]) {
    if (!Number.isInteger(raw[key])) issues.push(`${key}: expected an integer`);
  }
  if (!object(raw.metadata)) issues.push('metadata: expected an object');
  if (!pick(raw, 'metadata.version'))
    issues.push('metadata.version: schema version is missing');
  if (!pick(raw, 'metadata.product.name'))
    issues.push('metadata.product.name: product name is missing');
  if (!pick(raw, 'metadata.product.vendor_name'))
    issues.push('metadata.product.vendor_name: vendor name is missing');
  if (
    typeof raw.time !== 'number' ||
    !Number.isFinite(raw.time) ||
    Math.abs(raw.time) > 8.64e15
  )
    issues.push(
      'time: expected Unix milliseconds (time_dt is displayed as a fallback)',
    );
  if (!Object.hasOwn(severityNames, severity))
    issues.push('severity_id: outside the OCSF base enumeration');
  if (
    classId !== null &&
    typeof raw.activity_id === 'number' &&
    raw.type_uid !== classId * 100 + raw.activity_id
  )
    issues.push('type_uid: must equal class_uid × 100 + activity_id');
  if (def && raw.category_uid !== def.category_uid)
    issues.push(
      `category_uid: expected ${def.category_uid} for ${def.caption}`,
    );
  if (
    def?.attributes.activity_id?.enum &&
    !Object.hasOwn(def.attributes.activity_id.enum, str(raw.activity_id))
  )
    issues.push('activity_id: not in the bundled class enumeration');
  const source =
    pick(raw, 'metadata.product.name', 'metadata.log_name') || 'Unknown source';
  const actor =
    pick(
      raw,
      'actor.user.name',
      'user.name',
      'actor.user.uid',
      'user.uid',
      'actor.process.name',
      'src_endpoint.ip',
      'device.hostname',
    ) || '—';
  const target =
    pick(
      raw,
      'dst_endpoint.hostname',
      'dst_endpoint.ip',
      'file.path',
      'process.name',
      'device.hostname',
      'resources.0.name',
      'api.operation',
    ) || '—';
  const message =
    pick(
      raw,
      'finding_info.title',
      'message',
      'api.operation',
      'query.hostname',
    ) || `${className} · ${activity}`;
  const search =
    `${JSON.stringify(raw)} ${className} ${category} ${severityName} ${activity} ${status} ${source}`.toLowerCase();
  return {
    id,
    raw,
    time,
    classId,
    className,
    category,
    severity,
    severityName,
    activity,
    status,
    source,
    actor,
    target,
    message,
    issues,
    search,
  };
}
export function parseEvents(text: string, source = 'import'): ImportResult {
  if (new TextEncoder().encode(text).byteLength > MAX_BYTES)
    throw new Error('This import exceeds 25 MB. Split it into smaller files.');
  const input = text.replace(/^\uFEFF/, '').trim();
  if (!input)
    throw new Error(
      'No data found. Choose a JSON/NDJSON file or paste events.',
    );
  const result: ImportResult = {
    events: [],
    rejected: 0,
    problems: [],
    total: 0,
  };
  function problem(location: string, message: string) {
    result.rejected++;
    if (result.problems.length < 100)
      result.problems.push({ location, message });
  }
  function append(value: unknown, location: string) {
    result.total++;
    if (result.total > MAX_EVENTS)
      throw new Error(
        'This import exceeds 50,000 records. Split it into smaller files.',
      );
    if (!object(value)) {
      problem(location, 'Expected an event object.');
      return;
    }
    if (JSON.stringify(value).length > 1_000_000) {
      problem(location, 'Record exceeds the 1 MB per-event limit.');
      return;
    }
    result.events.push(normalize(value, `${source}:${result.total}`));
  }
  let parsed: unknown;
  let wholeDocument = true;
  try {
    parsed = JSON.parse(input);
  } catch {
    wholeDocument = false;
  }
  if (wholeDocument) {
    const values = Array.isArray(parsed)
      ? parsed
      : object(parsed) && Array.isArray(parsed.events)
        ? parsed.events
        : [parsed];
    values.forEach((value, index) => append(value, `Record ${index + 1}`));
  } else {
    if (input.startsWith('['))
      throw new Error(
        'Invalid JSON array. Check commas, brackets, and quotation marks.',
      );
    const lines = input.split(/\r?\n/);
    lines.forEach((line, index) => {
      if (!line.trim()) return;
      let value: unknown;
      try {
        value = JSON.parse(line);
      } catch {
        result.total++;
        if (result.total > MAX_EVENTS)
          throw new Error('This import exceeds 50,000 records.');
        problem(
          `Line ${index + 1}`,
          'Invalid JSON. Check syntax on this line.',
        );
        return;
      }
      append(value, `Line ${index + 1}`);
    });
  }
  if (!result.total) throw new Error('The file contains no events.');
  return result;
}
export type QueryToken = {
  field?: string;
  value: string;
  negative: boolean;
  operator: ':' | '=' | '>' | '<' | '>=' | '<=';
};
export function parseQuery(query: string): {
  tokens: QueryToken[];
  error?: string;
} {
  const parts = query.match(/(?:[^\s"\\]|\\.|"(?:\\.|[^"\\])*")+/g) || [];
  if ((query.match(/(?<!\\)"/g)?.length || 0) % 2)
    return {
      tokens: [],
      error: 'Close the quotation mark to run this search.',
    };
  const tokens: QueryToken[] = [];
  for (let part of parts) {
    const negative = part.startsWith('-');
    if (negative) part = part.slice(1);
    const match = part.match(/^([\w.]+)(>=|<=|:|=|>|<)(.*)$/);
    let value = match ? match[3] : part;
    if (value.startsWith('"') && value.endsWith('"'))
      value = value.slice(1, -1).replace(/\\(["\\])/g, '$1');
    if (!value)
      return { tokens: [], error: 'Add a value after the field operator.' };
    const operator = (match?.[2] || ':') as QueryToken['operator'];
    if (
      ['>', '<', '>=', '<='].includes(operator) &&
      !Number.isFinite(Number(value))
    )
      return { tokens: [], error: 'Numeric comparisons require a number.' };
    tokens.push({
      field: match?.[1],
      value: value.toLowerCase(),
      negative,
      operator,
    });
  }
  return { tokens };
}
export function matchesQuery(event: EventRecord, tokens: QueryToken[]) {
  return tokens.every((token) => {
    const aliases: Record<string, unknown> = {
      severity: event.severityName,
      class: event.className,
      source: event.source,
      actor: event.actor,
      target: event.target,
      status: event.status,
      category: event.category,
      activity: event.activity,
    };
    const value = token.field
      ? Object.hasOwn(aliases, token.field)
        ? aliases[token.field]
        : get(event.raw, token.field)
      : event.search;
    const text =
      (typeof value === 'object'
        ? JSON.stringify(value)
        : str(value)
      )?.toLowerCase() || '';
    let match = false;
    if (token.operator === ':')
      match = value !== undefined && text.includes(token.value);
    else if (token.operator === '=')
      match = value !== undefined && text === token.value;
    else {
      const number =
        typeof value === 'number'
          ? value
          : typeof value === 'string' && value.trim()
            ? Number(value)
            : NaN;
      const expected = Number(token.value);
      if (Number.isFinite(number))
        match =
          token.operator === '>'
            ? number > expected
            : token.operator === '<'
              ? number < expected
              : token.operator === '>='
                ? number >= expected
                : number <= expected;
    }
    return token.negative ? !match : match;
  });
}
export type Filters = {
  query: string;
  severities: number[];
  classes: string[];
  sources: string[];
  start: string;
  end: string;
  flagged: boolean;
  bookmarked: boolean;
};
export const emptyFilters: Filters = {
  query: '',
  severities: [],
  classes: [],
  sources: [],
  start: '',
  end: '',
  flagged: false,
  bookmarked: false,
};
export function filterEvents(
  events: EventRecord[],
  filters: Filters,
  bookmarks: string[] = [],
) {
  const query = parseQuery(filters.query);
  if (query.error) return [];
  const start = filters.start ? Date.parse(`${filters.start}Z`) : -Infinity;
  const end = filters.end ? Date.parse(`${filters.end}Z`) : Infinity;
  const marked = new Set(bookmarks);
  return events.filter(
    (event) =>
      (!filters.severities.length ||
        filters.severities.includes(event.severity)) &&
      (!filters.classes.length || filters.classes.includes(event.className)) &&
      (!filters.sources.length || filters.sources.includes(event.source)) &&
      (!filters.flagged || event.issues.length > 0) &&
      (!filters.bookmarked || marked.has(event.id)) &&
      ((!filters.start && !filters.end) ||
        (event.time !== null && event.time >= start && event.time <= end)) &&
      matchesQuery(event, query.tokens),
  );
}
export function histogram(events: EventRecord[], count = 48) {
  const timed = events.filter((e) => e.time !== null);
  if (!timed.length)
    return {
      start: 0,
      end: 0,
      width: 1,
      bins: [] as { start: number; end: number; count: number; high: number }[],
    };
  let start = Infinity,
    end = -Infinity;
  for (const e of timed) {
    start = Math.min(start, e.time!);
    end = Math.max(end, e.time!);
  }
  const width = Math.max(1000, Math.ceil((end - start + 1) / count));
  const bins = Array.from({ length: count }, (_, i) => ({
    start: start + i * width,
    end: start + (i + 1) * width - 1,
    count: 0,
    high: 0,
  }));
  for (const e of timed) {
    const b = bins[Math.min(count - 1, Math.floor((e.time! - start) / width))];
    b.count++;
    if (e.severity >= 4 && e.severity <= 6) b.high++;
  }
  return { start, end, width, bins };
}
export function csv(events: EventRecord[]) {
  const escape = (value: unknown) => {
    let text = str(value);
    if (/^[\s]*[=+@-]/.test(text)) text = "'" + text;
    return `"${text.replace(/"/g, '""')}"`;
  };
  return [
    [
      'time_utc',
      'severity',
      'class',
      'activity',
      'source',
      'actor',
      'target',
      'message',
    ],
    ...events.map((e) => [
      e.time === null ? '' : new Date(e.time).toISOString(),
      e.severityName,
      e.className,
      e.activity,
      e.source,
      e.actor,
      e.target,
      e.message,
    ]),
  ]
    .map((row) => row.map(escape).join(','))
    .join('\r\n');
}
export function flatten(
  value: unknown,
  prefix = '',
  depth = 0,
): { path: string; value: unknown }[] {
  if (depth > 12) return [{ path: prefix, value: '[Expand in original JSON]' }];
  if (value !== null && typeof value === 'object') {
    const entries = Object.entries(value);
    if (!entries.length)
      return [{ path: prefix, value: Array.isArray(value) ? '[]' : '{}' }];
    return entries.flatMap(([key, item]) =>
      flatten(item, prefix ? `${prefix}.${key}` : key, depth + 1),
    );
  }
  return [{ path: prefix, value }];
}

export function sortEvents(events: EventRecord[], ascending = false) {
  return [...events].sort((a, b) => {
    if (a.time === null) return b.time === null ? 0 : 1;
    if (b.time === null) return -1;
    return (ascending ? 1 : -1) * (a.time - b.time);
  });
}
