import assert from 'node:assert/strict';
const origin = process.argv[2] || 'http://127.0.0.1:8080';
const revision = process.argv[3];
async function get(path) {
  const response = await fetch(new URL(path, origin), {
    signal: AbortSignal.timeout(15000),
  });
  assert.equal(response.status, 200, `${path}: HTTP ${response.status}`);
  return response;
}
const health = await (await get('/healthz')).json();
assert.equal(health.status, 'ok');
if (revision) assert.equal(health.revision, revision);
const html = await (await get('/')).text();
assert.match(html, /OCSF/);
assert.match(html, /Sign in to OCSF Explorer/);
assert.doesNotMatch(html, /Paste your access token/);
const authResponse = await get('/auth/session');
assert.match(authResponse.headers.get('cache-control') || '', /no-store/);
const auth = await authResponse.json();
assert.equal(typeof auth.enabled, 'boolean');
if (auth.enabled) {
  assert.ok(
    auth.client_id && auth.nonce,
    'Missing Google sign-in configuration',
  );
  const cookie = authResponse.headers.get('set-cookie') || '';
  assert.match(cookie, /__Host-siem-login=/);
  assert.match(cookie, /HttpOnly/i);
  assert.match(cookie, /Secure/i);
}
const crossSite = await fetch(new URL('/auth/google', origin), {
  method: 'POST',
  headers: {
    Origin: 'https://evil.example',
    'Content-Type': 'application/json',
  },
  body: JSON.stringify({ credential: 'forged' }),
});
assert.equal(crossSite.status, auth.enabled ? 403 : 503);
const assets = [
  ...html.matchAll(/(?:src|href)="([^"\s]+\.(?:js|css)(?:\?[^"\s]*)?)"/g),
].map((m) => m[1]);
assert.ok(assets.length > 0, 'No built JS/CSS assets in HTML');
for (const path of new Set(assets)) {
  const response = await get(path);
  assert.doesNotMatch(response.headers.get('content-type') || '', /text\/html/);
}
assert.equal((await fetch(new URL('/missing-asset.js', origin))).status, 404);
console.log(
  `Smoke passed: ${origin}, revision ${health.revision}, ${assets.length} assets`,
);
