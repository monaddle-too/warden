import test from 'node:test';
import assert from 'node:assert/strict';
import {
  catalog,
  csv,
  emptyFilters,
  eventTime,
  filterEvents,
  flatten,
  get,
  histogram,
  MAX_EVENTS,
  normalize,
  parseEvents,
  parseQuery,
  sortEvents,
  type JsonObject,
} from '../lib/events.ts';
const event = (patch: JsonObject = {}) => ({
  class_uid: 3002,
  category_uid: 3,
  activity_id: 1,
  type_uid: 300201,
  severity_id: 4,
  time: 1788964321000,
  status_id: 2,
  metadata: {
    version: '1.6.0',
    product: { name: 'Identity provider', vendor_name: 'Example' },
  },
  user: { name: 'Alice' },
  message: 'Authentication failure',
  ...patch,
});
void test('resolves ID-only OCSF events with the pinned dictionary without changing raw data', () => {
  const raw = event();
  const before = JSON.stringify(raw);
  const result = normalize(raw, 'one');
  assert.equal(result.className, 'Authentication');
  assert.equal(result.activity, 'Logon');
  assert.equal(result.category, 'Identity & Access Management');
  assert.equal(result.severityName, 'High');
  assert.equal(result.status, 'Failure');
  assert.equal(result.actor, 'Alice');
  assert.deepEqual(result.issues, []);
  assert.equal(JSON.stringify(raw), before);
});
void test('imports single objects, arrays, envelopes, CRLF NDJSON and BOM files', () => {
  const raw = event();
  for (const input of [
    JSON.stringify(raw),
    JSON.stringify([raw]),
    JSON.stringify({ events: [raw] }),
    '\uFEFF' + JSON.stringify(raw),
  ])
    assert.equal(parseEvents(input).events.length, 1);
  assert.equal(
    parseEvents(`${JSON.stringify(raw)}\r\n\r\n${JSON.stringify(raw)}`).events
      .length,
    2,
  );
});
void test('reports malformed records with their original line number and keeps valid neighbors', () => {
  const result = parseEvents(
    `${JSON.stringify(event())}\n{bad}\n\nnull\n${JSON.stringify(event())}`,
  );
  assert.equal(result.events.length, 2);
  assert.equal(result.total, 4);
  assert.equal(result.rejected, 2);
  assert.deepEqual(
    result.problems.map((p) => p.location),
    ['Line 2', 'Line 4'],
  );
});
void test('wrong-shaped array entries are rejected while incomplete event objects remain inspectable', () => {
  const result = parseEvents('[null,2,"x",{},[]]');
  assert.equal(result.rejected, 4);
  assert.equal(result.events.length, 1);
  assert.ok(result.events[0].issues.length >= 7);
});
void test('does not silently truncate a dataset above the supported event count', () => {
  assert.throws(
    () =>
      parseEvents(
        '[' +
          Array(MAX_EVENTS + 1)
            .fill('null')
            .join(',') +
          ']',
      ),
    /50,000/,
  );
});
void test('bounds import and per-event sizes and caps diagnostic lists', () => {
  assert.throws(() => parseEvents(' '.repeat(26 * 1024 * 1024)), /25 MB/);
  const result = parseEvents(
    JSON.stringify([event({ message: 'x'.repeat(1_000_001) }), event()]),
  );
  assert.equal(result.rejected, 1);
  assert.equal(result.events.length, 1);
  const problems = parseEvents(Array(150).fill('invalid').join('\n'));
  assert.equal(problems.rejected, 150);
  assert.equal(problems.problems.length, 100);
});
void test('empty and invalid array documents return useful errors', () => {
  assert.throws(() => parseEvents(''), /No data/);
  assert.throws(() => parseEvents('[]'), /no events/);
  assert.throws(() => parseEvents('[{]'), /Invalid JSON array/);
});
void test('unknown classes and custom fields survive round trips', () => {
  const raw = event({
    class_uid: 900001,
    category_uid: 9,
    type_uid: 90000101,
    unmapped: { custom_key: [false, 0, null, '🛡️'] },
  });
  const result = parseEvents(JSON.stringify(raw));
  assert.equal(result.events[0].className, 'Class 900001');
  assert.deepEqual(result.events[0].raw, raw);
});
void test('flags contradictory classification, missing metadata, invalid enums and non-millisecond types', () => {
  const result = normalize(
    event({
      type_uid: 300299,
      category_uid: 4,
      severity_id: 98,
      time: '2026-09-09',
      metadata: {},
    }),
    'bad',
  );
  assert.ok(result.issues.some((s) => s.startsWith('type_uid:')));
  assert.ok(result.issues.some((s) => s.startsWith('category_uid:')));
  assert.ok(result.issues.some((s) => s.startsWith('time:')));
  assert.ok(result.issues.some((s) => s.startsWith('severity_id:')));
});
void test('time handling keeps epoch zero and distinguishes absent time from an ISO fallback', () => {
  assert.equal(eventTime({ time: 0 }), 0);
  assert.equal(eventTime({}), null);
  assert.equal(eventTime({ time: Infinity }), null);
  assert.equal(eventTime({ time: 9e15 }), null);
  assert.equal(eventTime({ time_dt: '2026-09-09T00:00:00Z' }), 1788912000000);
});
void test('combines phrases, aliases, nested fields, numeric comparisons, and negation', () => {
  const rows = [
    normalize(
      event({ src_endpoint: { ip: '198.51.100.24' }, severity_id: 5 }),
      'a',
    ),
    normalize(
      event({
        severity_id: 1,
        status_id: 1,
        user: { name: 'Bob' },
        metadata: {
          version: '1.6.0',
          product: { name: 'Different provider', vendor_name: 'Example' },
        },
      }),
      'b',
    ),
  ];
  for (const query of [
    '"authentication failure" actor:alice',
    'class:authentication severity_id>=4 -status:success',
    'src_endpoint.ip=198.51.100.24',
    'severity:critical',
    'metadata.product.name="Identity provider"',
  ])
    assert.deepEqual(
      filterEvents(rows, { ...emptyFilters, query }).map((e) => e.id),
      ['a'],
    );
  assert.equal(
    filterEvents(rows, { ...emptyFilters, query: 'missing>=0' }).length,
    0,
  );
  assert.equal(
    filterEvents(rows, { ...emptyFilters, query: 'severity_id<4' }).length,
    1,
  );
});
void test('incomplete and invalid search syntax never broadens the result set', () => {
  for (const query of ['actor:"Alice', 'class:', 'severity_id>high']) {
    assert.ok(parseQuery(query).error);
    assert.equal(
      filterEvents([normalize(event(), 'a')], { ...emptyFilters, query })
        .length,
      0,
    );
  }
});
void test('escaped quotation marks and backslashes work in exact field queries', () => {
  const name = 'a"b\\c';
  const row = normalize(event({ user: { name } }), 'a');
  assert.equal(
    filterEvents([row], {
      ...emptyFilters,
      query: `actor=${JSON.stringify(name)}`,
    }).length,
    1,
  );
});
void test('facets, UTC time bounds, bookmarks and quality checks compose', () => {
  const rows = [
    normalize(event({ time: 1788912000000 }), 'a'),
    normalize(event({ time: 1788912001000 }), 'b'),
    normalize(event({ time: undefined }), 'c'),
  ];
  assert.deepEqual(
    filterEvents(rows, {
      ...emptyFilters,
      start: '2026-09-09T00:00:00',
      end: '2026-09-09T00:00:00',
      severities: [4],
      sources: ['Identity provider'],
      classes: ['Authentication'],
    }).map((e) => e.id),
    ['a'],
  );
  assert.deepEqual(
    filterEvents(rows, { ...emptyFilters, bookmarked: true }, ['b']).map(
      (e) => e.id,
    ),
    ['b'],
  );
  assert.deepEqual(
    filterEvents(rows, { ...emptyFilters, flagged: true }).map((e) => e.id),
    ['c'],
  );
});
void test('time histogram counts every event once, including equal timestamps and epoch zero', () => {
  const rows = [
    normalize(event({ time: 0 }), 'a'),
    normalize(event({ time: 0, severity_id: 99 }), 'b'),
    normalize(event({ time: 48000 }), 'c'),
    normalize(event({ time: undefined }), 'd'),
  ];
  const chart = histogram(rows);
  assert.equal(
    chart.bins.reduce((n, b) => n + b.count, 0),
    3,
  );
  assert.equal(
    chart.bins.reduce((n, b) => n + b.high, 0),
    2,
  );
  assert.equal(chart.start, 0);
  assert.deepEqual(histogram([]).bins, []);
});
void test('CSV quotes cells and neutralizes formula injection including whitespace prefixes', () => {
  const output = csv([
    normalize(
      event({
        message: '\t=WEBSERVICE("https://example.com")',
        user: { name: 'x,y' },
      }),
      'a',
    ),
  ]);
  assert.ok(output.includes('"\'\t=WEBSERVICE(""https://example.com"")"'));
  assert.ok(output.includes('"x,y"'));
  assert.equal(output.split('\r\n').length, 2);
});
void test('field navigation preserves false, zero, null and array paths without reading prototype fields', () => {
  const data = { items: [{ enabled: false, size: 0, extra: null }], empty: [] };
  assert.equal(get(data, 'items.0.enabled'), false);
  assert.equal(get({}, 'constructor'), undefined);
  assert.equal(get({}, '__proto__'), undefined);
  const fields = flatten(data);
  assert.ok(fields.some((f) => f.path === 'items.0.size' && f.value === 0));
  assert.ok(fields.some((f) => f.path === 'items.0.extra' && f.value === null));
});
void test('bundled schema contains all six demo classes and extension IDs', () => {
  assert.equal(catalog.version, '1.6.0');
  for (const uid of [1007, 2004, 3002, 4001, 4003, 6003])
    assert.ok(catalog.classes[uid]);
  assert.ok(Object.keys(catalog.classes).length >= 80);
});

void test('sorts timestamps stably and keeps missing times last in both directions', () => {
  const rows = [
    normalize(event({ time: 2000 }), 'a'),
    normalize(event({ time: undefined }), 'b'),
    normalize(event({ time: 1000 }), 'c'),
  ];
  assert.deepEqual(
    sortEvents(rows).map((e) => e.id),
    ['a', 'c', 'b'],
  );
  assert.deepEqual(
    sortEvents(rows, true).map((e) => e.id),
    ['c', 'a', 'b'],
  );
  assert.deepEqual(
    rows.map((e) => e.id),
    ['a', 'b', 'c'],
  );
});
