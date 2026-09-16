# Google Docs connection

Warden brokers read-only Google Docs REST requests using OAuth credentials held
in its trusted control-plane process. This extends the HTTP approval proxy;
it does not create an MCP server or forward Google's hosted MCP.

## User connection flow

The Warden operator registers one OAuth web client in Google Cloud, enables the
Docs API, and configures the client ID/secret on the host. End users do not create
clients, paste grants, or handle tokens: Connect Google Docs opens Google's
account/consent flow; Google redirects an authorization code to Warden; Warden
exchanges it automatically and refreshes access tokens. Google consent is still
required for personal accounts. The operator setup form is under a collapsed
setup section in this local prototype, not a per-user onboarding requirement.

This account's client is named Warden Google Docs in the existing Monaddle
project, as requested. Google consent therefore displays the project's existing
Monaddle branding. Other clients and project branding were not changed.
The project is in Testing, and the owner's account is an eligible test user.
Testing refresh tokens for this scope expire after seven days. Production
verification/public onboarding was not performed.

Scope: https://www.googleapis.com/auth/documents.readonly . Google authorizes
reads across the account's Docs; Warden enforces document and operation approval.
No Drive listing, editing, sharing, comments, or write scope is requested.

## Supported requests

`GET https://docs.googleapis.com/v1/documents/DOCUMENT_ID`

Optional query parameters: includeTabsContent=true/false and suggestionsViewMode
with Google's documented enum values. Use includeTabsContent=true to retrieve
all document tabs. Unknown query parameters, fields masks, batch routes, writes,
nonempty GET bodies, non-HTTPS, and nonstandard ports are rejected.
Document IDs use letters, digits, underscores, and hyphens (maximum 256).
The optional allowed_google_documents policy list restricts which documents can
be approved; empty denies all documents. It is independent of GitHub/Figma lists.

Unapproved requests return 428. Approve in Warden and retry the exact request.
Exact grants are single-use; repeated grants match the operation and full URL.
Caller Authorization, cookies, X-Goog-Api-Key, and X-Goog-User-Project are removed.
Only an approved request receives Google's bearer token. No redirect is followed.
Google, Figma, and GitHub credentials and grants remain separate.

OAuth uses PKCE and single-use expiring state; token exchange uses the fixed
HTTPS oauth2.googleapis.com/token endpoint. The callback validates Google's
optional issuer field. Unexpected token scopes fail closed. Secrets are redacted
and memory-only, matching the existing prototype: restarting requires operator
configuration and account reconnection. Disconnect removes local credentials and
revokes local Google grants, but does not revoke consent at Google.

## Current runtime and validation

Google control plane: http://127.0.0.1:18772/ . Registered callback:
http://127.0.0.1:18772/oauth/google_docs/callback . State is in .local/google-state.
The existing Figma instance on port 18769 was kept running to preserve its
memory-only connection. This does not automatically connect separate SBX Engines.
A temporary loopback proxy on port 18773 was used for acceptance; this is not a
hostile-sandbox containment demonstration or multi-tenant deployment.

Live OAuth and document read passed on September 14, 2026 using Snippet
Modernization: Recommended Work and Disposition. Results: unapproved read 428,
exact-approved read 200, replay 428, other document 428, batchUpdate POST 403.
Bogus caller credentials were discarded. No document was edited. Test grants
were revoked and pending test requests denied. Sanitized evidence is in
.local/google-state/live-acceptance.json; document content was not saved.

The live HTTP/2 test exposed a shared proxy bug: writing a literal Host header
alongside HTTP/2 authority caused an upstream protocol error. The proxy now uses
mitmproxy's protocol-aware host_header setter; default curl and HTTP/1.1 acceptance
both pass. A proxy regression covers authority and Host handling.

137 relevant unit/integration tests passed, including existing GitHub/Figma/SBX
regressions. JavaScript syntax and git whitespace checks passed.

## Local mode

A local installation needs no per-deployment Google client. When
`warden-policy` runs without `--google-config` and the release carries the
Warden Desktop client (`release.GoogleDocsClientID` and its non-confidential
secret), the connection is configured with that client and the loopback
redirect `http://127.0.0.1:<chat port>/oauth/google_docs/callback`, the port
coming from `--chat-listen` (default `127.0.0.1:18780`). Google redirects the
browser straight to `warden-chat`, which serves that path itself and forwards
the code to the policy service as the sharing `callback` operation; the edge
is not involved, and the chat listener must stay on `127.0.0.1` because the
handler checks the `Host` header. An operator file given with
`--google-config` always overrides the built-in client. When the release
constant is empty and no file is given, the connection reports
`configured:false` and the UI hides it. The connection record in
`google.sqlite` now names the principal that connected (`owner` locally).

## References

- https://developers.google.com/workspace/docs/api/auth
- https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/get
- https://developers.google.com/identity/protocols/oauth2/web-server
- https://developers.google.com/identity/protocols/oauth2
