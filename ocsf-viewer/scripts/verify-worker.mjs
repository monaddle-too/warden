// Execute the actual production Web Worker bundle with a Node message-port shim.
import { Worker } from 'node:worker_threads';
import { readdir } from 'node:fs/promises';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import assert from 'node:assert/strict';
const folder = resolve('dist/client/_next/static/workers');
const file = (await readdir(folder)).find((name) =>
  name.startsWith('import.worker-'),
);
if (!file) throw new Error('Build first: import worker bundle is missing.');
const url = pathToFileURL(resolve(folder, file)).href;
const worker = new Worker(
  `
const {parentPort} = require('node:worker_threads');
globalThis.self = {postMessage: value => parentPort.postMessage(value)};
import(${JSON.stringify(url)}).then(() => {
  parentPort.on('message', data => self.onmessage({data}));
  parentPort.postMessage({ready: true});
});
`,
  { eval: true },
);
const next = () =>
  new Promise((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error('Worker timed out.')),
      30000,
    );
    worker.once('message', (message) => {
      clearTimeout(timer);
      resolve(message);
    });
    worker.once('error', reject);
  });
try {
  await next();
  const event = {
    class_uid: 3002,
    category_uid: 3,
    type_uid: 300201,
    activity_id: 1,
    severity_id: 4,
    time: 1788912000000,
    metadata: {
      version: '1.6.0',
      product: { name: 'Test', vendor_name: 'Example' },
    },
  };
  let response = next();
  worker.postMessage({
    text: JSON.stringify(event) + '\ninvalid\n' + JSON.stringify(event),
    source: 'worker-test',
  });
  const small = await response;
  assert.equal(small.result.events.length, 2);
  assert.equal(small.result.rejected, 1);
  assert.equal(small.result.events[0].className, 'Authentication');
  response = next();
  worker.postMessage({ text: '[broken', source: 'worker-error' });
  assert.match((await response).error, /Invalid JSON array/);
  response = next();
  const start = performance.now();
  worker.postMessage({
    text: Array(50_000).fill(JSON.stringify(event)).join('\n'),
    source: 'worker-load',
  });
  const large = await response;
  assert.equal(large.result.events.length, 50_000);
  assert.equal(large.result.rejected, 0);
  assert.equal(
    new Set(large.result.events.map((event) => event.id)).size,
    50_000,
  );
  console.log(
    `Production import worker passed valid/malformed inputs and 50,000 events in ${Math.round(performance.now() - start)} ms.`,
  );
} finally {
  await worker.terminate();
}
