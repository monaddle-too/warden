import { readFile, writeFile } from 'node:fs/promises';
import assert from 'node:assert/strict';
const catalog = JSON.parse(
  await readFile(new URL('../lib/catalog.json', import.meta.url), 'utf8'),
);
const compact = Object.fromEntries(
  Object.entries(catalog.classes).map(([id, c]) => [
    id,
    {
      category: c.category_uid,
      activities: Object.keys(c.attributes.activity_id?.enum || {}),
    },
  ]),
);
const target = new URL(
  '../backend/internal/pipeline/catalog.json',
  import.meta.url,
);
if (process.argv.includes('--check'))
  assert.deepEqual(
    JSON.parse(await readFile(target, 'utf8')),
    compact,
    'Run node scripts/sync-contract.mjs after updating the schema',
  );
else await writeFile(target, JSON.stringify(compact) + '\n');
