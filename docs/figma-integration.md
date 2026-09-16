# Figma account connection

Warden can broker selected Figma REST reads using a host-held OAuth connection.
This extends the existing GitHub HTTP approval path. It does not implement a
new MCP server, proxy Figma's hosted MCP, or change the user's Figma sharing.

## Supported requests

Only HTTPS on `api.figma.com:443`, with GET and no request body:

- `/v1/me`
- `/v1/files/FILE_KEY` (optional `depth`, `ids`, `version`)
- `/v1/files/FILE_KEY/nodes` (`ids` required; optional `depth`, `version`)
- `/v1/files/FILE_KEY/meta`
- `/v1/files/FILE_KEY/comments` (optional `as_md`)

Other Figma operations, writes, OAuth endpoints from agents, and Figma website
and hosted MCP channels are denied by the credential proxy. Unknown parameters
fail closed. A file-content response can contain child nodes and references;
this is file/operation authorization, not node-level data filtering. Images and
other URLs returned in a file response are not automatically granted access.

## Connect an account

1. Create a Figma OAuth app at https://www.figma.com/developers/apps. Choose an
   appropriate owner and publish privately for that team, or use draft testing
   when the authorizing account is eligible. Public apps require Figma review.
2. Configure the exact callback shown in **Warden → Policy & credentials → Figma connection**.
   The isolated local instance prepared for this task uses
   `http://127.0.0.1:18769/oauth/figma/callback`.
3. Request only `current_user:read`, `file_content:read`, `file_metadata:read`,
   and `file_comments:read`.
4. Enter the app client ID and secret into Warden's authenticated setup form.
   These are app registration credentials; subsequent account connections use
   Figma's browser consent flow, not a pasted personal access token.
5. Click **Connect Figma**, authorize in Figma, then return to Warden.

The app secret, access token, refresh token, and pending PKCE verifier are held
only in host memory. Restarting the control plane requires app configuration and
account connection again, matching the prototype's memory-only credential model.
No Figma credentials are copied from the Codex connector or browser session.
This initial local implementation does not provide durable encrypted credential
storage or a shared, multi-tenant connection service.

## Agent requests and human grants

An agent uses the existing enforced HTTP proxy and calls a supported Figma URL
without a Figma credential. The proxy removes incoming Authorization, cookies,
and `X-Figma-Token`; the host returns HTTP 428 until the owner approves the request
in Warden. The caller explicitly retries. Only then does the trusted proxy attach
the host's Figma bearer token. Figma and GitHub operation IDs and credentials are
separate. Scoped grants cover one operation and the exact URL including query;
exact grants are single-use. Provider scopes and the account's upstream access
still apply. Read responses retain the existing 16 MiB buffered-response limit.

The optional `allowed_figma_files` policy array restricts file keys independently
of `allowed_repositories`. An empty array denies every file; `/v1/me` still needs
its own approval. Omit the array to allow the owner to approve any accessible file.
Disconnecting or successfully replacing the connection invalidates Figma grants
and pending requests. Disconnect removes local credentials; it does not revoke
consent at Figma. Figma grants also expire and are revoked on restart as before.

Both proxy variants classify Figma as protected. SBX destination admission lets
`api.figma.com` reach the approval path, but each sandbox still uses its separate
Engine, credentials, active lease, and verified network binding. Configuring the
standalone control plane does not automatically connect an SBX engine. Shared
account onboarding across SBX instances and MCP client onboarding are not part
of this patch. Do not claim a live sandbox demonstration from the unit tests.

## Verification

Run from the repository with Python and the pinned proxy dependencies installed:

```sh
PYTHONPATH=host:tests python -m unittest test_core test_server test_proxy test_repository test_sbx test_sbx_proxy test_figma test_figma_proxy test_figma_server -q
node --check host/web/app.js
git diff --check
```

The 111-test suite passed on September 14, 2026. It covers token/provider isolation,
exact and URL-scoped grants, file allowlists, denied writes, canonical request
checks, header stripping, refresh failure, memory-only storage, callback replay,
state expiry, PKCE, owner/Origin checks, disconnect/reconnection revocation, and
existing GitHub/proxy/SBX behavior. Proxy tests use real mitmproxy flow objects
with controlled upstream and authorization transports.

Live Figma OAuth and upstream access passed on September 14, 2026 using the
existing **Connection Test Mockup**. The actual loopback HTTP proxy returned 428
before approval, 200 after an exact grant, 428 on replay and a different file,
and 403 for a comment write. Supplied guest credentials were discarded. No Figma
file content was changed, and test grants were revoked. This is not a live SBX
containment test. A discovered Figma load-balancer incompatibility was fixed:
the proxy now omits Content-Length: 0 on Figma GETs; 18 proxy tests passed after
the fix.

The Warden OAuth app is configured under Daniel's team and the owner's account
is connected. After the owner enabled two-factor authentication, the app was
published privately on September 14, 2026. Figma confirms that Daniel's team
members can access it. The four scopes remain read-only.

## References

- https://developers.figma.com/docs/rest-api/oauth-apps/
- https://developers.figma.com/docs/rest-api/scopes/
- https://developers.figma.com/docs/rest-api/file-endpoints/
