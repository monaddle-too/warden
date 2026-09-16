// Run with the sibling viewer's pinned Nix Node runtime. No viewer files change.
import { readFileSync } from 'node:fs';
import assert from 'node:assert/strict';
import { parseEvents } from '../ocsf-viewer/lib/events.ts';
const input = readFileSync(process.argv[2], 'utf8');
const result = parseEvents(input, 'warden');
assert.equal(result.rejected, 0, 'viewer rejected records');
assert.ok(result.events.length > 0);
const issues = result.events.flatMap(e => e.issues.map(issue => `${e.raw.metadata.uid}: ${issue}`));
assert.deepEqual(issues, [], 'viewer field diagnostics');
assert.ok(result.events.every(e => e.source === 'Warden'));
console.log(`Viewer accepted ${result.events.length} Warden events with zero field issues.`);
