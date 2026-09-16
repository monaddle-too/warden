import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
const base = 'http://127.0.0.1:8080';
const admin = process.env.OCSF_ADMIN_TOKEN,
  read = process.env.OCSF_READ_TOKEN,
  ingest = process.env.OCSF_INGEST_TOKEN;
const compose = (...args) =>
  execFileSync(
    'docker',
    ['compose', '-p', 'ocsf-smoke', '-f', 'deploy/compose.yaml', ...args],
    { stdio: 'inherit' },
  );
async function api(path, options = {}, token = admin) {
  const r = await fetch(base + path, {
    ...options,
    headers: { Authorization: `Bearer ${token}`, ...options.headers },
    signal: AbortSignal.timeout(25000),
  });
  return { status: r.status, data: await r.json() };
}
const now = Date.now();
const event = (n) => ({
  time: now + n,
  class_uid: 3002,
  category_uid: 3,
  activity_id: 1,
  type_uid: 300201,
  severity_id: 2,
  metadata: {
    version: '1.6.0',
    product: { name: 'CI fixture', vendor_name: 'OCSF Explorer' },
  },
  message: `recovery fixture ${n} DB::Exception: harmless log text`,
  src_endpoint: { ip: '10.0.0.1' },
  custom: { keep: false, n },
});
const post = (key, body) =>
  api(
    '/api/v1/events',
    {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-ndjson',
        'Idempotency-Key': key,
        'X-OCSF-Source': 'ci',
      },
      body,
    },
    ingest,
  );
async function until(fn, label) {
  for (let n = 0; n < 90; n++) {
    try {
      if (await fn()) return;
    } catch {}
    await delay(1000);
  }
  throw new Error(`Timed out: ${label}`);
}
async function indexed(id) {
  await until(async () => {
    const r = await api(`/api/v1/batches/${id}`);
    return r.data.state === 'indexed';
  }, `indexed ${id}`);
}
assert.equal((await api('/api/v1/events', {}, 'bad')).status, 401);
assert.equal((await api('/api/v1/events', {}, ingest)).status, 403);
assert.equal((await api('/api/v1/batches', {}, read)).status, 403);
assert.equal(
  (await api('/api/v1/events', { method: 'POST' }, read)).status,
  403,
);
const raw = JSON.stringify(event(0)),
  body = [
    raw,
    'not json',
    JSON.stringify(event(1)),
    JSON.stringify(event(2)),
  ].join('\n');
let receipt = await post('first', body);
assert.equal(receipt.status, 202);
assert.equal(receipt.data.accepted, 3);
assert.equal(receipt.data.rejected, 1);
const firstID = receipt.data.id;
await indexed(firstID);
const retry = await post('first', body);
assert.equal(retry.data.id, firstID);
assert.equal(retry.data.duplicate, true);
assert.equal((await post('first', raw)).status, 409);
const result = await api('/api/v1/events?stream=ci&limit=2', {}, read);
assert.equal(result.status, 200);
assert.equal(result.data.total, 3);
assert.equal(result.data.events.length, 2);
assert.ok(result.data.next_cursor);
const second = await api(
  `/api/v1/events?stream=ci&limit=2&cursor=${result.data.next_cursor}`,
  {},
  read,
);
assert.equal(second.data.events.length, 1);
assert.equal(second.data.events[0].raw, raw);
assert.equal(
  new Set([...result.data.events, ...second.data.events].map((e) => e.id)).size,
  3,
);
assert.equal(
  (await api(`/api/v1/events?stream=changed&cursor=${result.data.next_cursor}`))
    .status,
  400,
);
assert.equal(
  (await api('/api/v1/events?field=src_endpoint.ip&value=10.0.0.1')).data.total,
  3,
);
assert.equal(
  (await api('/api/v1/events?field=custom.keep&value=false')).data.total,
  3,
);
assert.equal(
  (await api('/api/v1/events?q=' + encodeURIComponent("' OR 1=1 --"))).data
    .total,
  0,
);
const batch = await api(`/api/v1/batches/${firstID}`);
assert.equal(
  Buffer.from(batch.data.rejections[0].raw_base64, 'base64').toString(),
  'not json',
);
compose('stop', 'clickhouse');
await until(
  async () => !(await api('/api/v1/status')).data.storage_ready,
  'storage offline',
);
receipt = await post('offline', JSON.stringify(event(3)));
assert.equal(receipt.status, 202);
assert.equal(receipt.data.state, 'queued');
const snapshot = await fetch(base + '/api/v1/backup', {
  headers: { Authorization: `Bearer ${admin}` },
});
assert.equal(snapshot.status, 200);
const snapshotPath = join(process.env.RUNNER_TEMP, 'ocsf-queue-snapshot.db');
await writeFile(snapshotPath, Buffer.from(await snapshot.arrayBuffer()), {
  mode: 0o600,
});
compose('restart', 'app');
await until(
  async () => (await api('/api/v1/status')).status === 200,
  'app restarted while storage offline',
);
assert.equal(
  (await post('offline', JSON.stringify(event(3)))).data.duplicate,
  true,
);
compose('up', '-d', '--wait', '--wait-timeout', '120', 'clickhouse');
await indexed(receipt.data.id);
assert.equal((await api('/api/v1/events?stream=ci')).data.total, 4);
// Replay an already indexed batch at the storage layer to model a crash after
// ClickHouse commits and before the queue receipt commits. FINAL must deduplicate.
const rows = (await api('/api/v1/events?stream=ci')).data.events;
execFileSync(
  'docker',
  [
    'compose',
    '-p',
    'ocsf-smoke',
    '-f',
    'deploy/compose.yaml',
    'exec',
    '-T',
    'clickhouse',
    'sh',
    '-c',
    'exec clickhouse-client --user ocsf --password "$CLICKHOUSE_PASSWORD" --query "INSERT INTO ocsf.events (id,batch_id,time,received_ms,class_uid,severity_id,source,stream,raw) FORMAT JSONEachRow"',
  ],
  {
    input: rows.map((r) => JSON.stringify(r)).join('\n'),
    stdio: ['pipe', 'inherit', 'inherit'],
  },
);
assert.equal((await api('/api/v1/events?stream=ci')).data.total, 4);
// Exercise export restoration into an empty table in this disposable CI stack.
compose('stop', 'app');
compose(
  'exec',
  '-T',
  'clickhouse',
  'sh',
  '-c',
  'exec clickhouse-client --user ocsf --password "$CLICKHOUSE_PASSWORD" --query "TRUNCATE TABLE ocsf.events"',
);
execFileSync(
  'docker',
  [
    'compose',
    '-p',
    'ocsf-smoke',
    '-f',
    'deploy/compose.yaml',
    'exec',
    '-T',
    'clickhouse',
    'sh',
    '-c',
    'exec clickhouse-client --user ocsf --password "$CLICKHOUSE_PASSWORD" --query "INSERT INTO ocsf.events (id,batch_id,time,received_ms,class_uid,severity_id,source,stream,raw) FORMAT JSONEachRow"',
  ],
  {
    input: rows.map((r) => JSON.stringify(r)).join('\n'),
    stdio: ['pipe', 'inherit', 'inherit'],
  },
);
// Restore the earlier queue snapshot after indexing. Its queued event overlaps
// the stored export and must replay to the same visible ID after restoration.
compose('stop', 'app');
execFileSync('sudo', [
  'install',
  '-m',
  '600',
  '-o',
  '65532',
  '-g',
  '65532',
  snapshotPath,
  join(process.env.OCSF_DATA_DIR, 'queue.db'),
]);
compose('up', '-d', '--wait', '--wait-timeout', '120', 'app');
await indexed(receipt.data.id);
assert.equal((await api('/api/v1/events?stream=ci')).data.total, 4);
const backup = await fetch(base + '/api/v1/backup', {
  headers: { Authorization: `Bearer ${admin}` },
});
assert.equal(backup.status, 200);
assert.ok((await backup.arrayBuffer()).byteLength > 4096);
const largeBody = Array.from({ length: 36 }, (_, n) =>
  JSON.stringify({
    ...event(100 + n),
    message: 'x'.repeat(200000),
    metadata: {
      version: '1.6.0',
      product: { name: 'Large CI fixture', vendor_name: 'OCSF Explorer' },
    },
  }),
).join('\n');
const largeReceipt = await post('large-page', largeBody);
assert.equal(largeReceipt.status, 202);
await indexed(largeReceipt.data.id);
const largePage = await api(
  '/api/v1/events?source=Large%20CI%20fixture&limit=200',
);
assert.equal(largePage.status, 200);
assert.equal(largePage.data.total, 36);
assert.ok(largePage.data.events.length < 36);
assert.ok(largePage.data.next_cursor);
const remaining = await api(
  '/api/v1/events?source=Large%20CI%20fixture&limit=200&cursor=' +
    largePage.data.next_cursor,
);
assert.equal(
  new Set([...largePage.data.events, ...remaining.data.events].map((e) => e.id))
    .size,
  36,
);
console.log(
  'PASS: auth, mixed validation, raw preservation, idempotency, cursor pagination, filters, quarantine, database outage, app restart, recovery, deduplication, queue backup and restored-queue replay',
);
