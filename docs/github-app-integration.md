# Shared GitHub App credentials

Warden can use the existing Monaddle Workspace GitHub App instead of a pasted
PAT or user OAuth token. GitHub still uses a token on the wire: the trusted broker
creates a short-lived installation token automatically after a Warden grant
matches, and the trusted proxy attaches it. Agents never receive app secrets or
installation tokens. Actions use the installation's bot identity, not a person.

## Existing account setup

The registered app is Monaddle Workspace (4893871), installed as 160513175 for
monaddle-too. Its existing selected repository is monaddle-too/panta.
Repository selection and app permissions were not changed.

The private broker runs on OVH, at
/opt/warden-github-broker/github_app.py. It reads only the `app` record from the
existing Panta store /opt/workspace/data/GitHub/github.sqlite and decrypts it using
the existing private encryption.key. It does not read user token records, alter
Panta's database, restart Panta, generate another app key, or copy the private
key to the Mac. The existing app ID and slug are validated before use.

Warden's local owner-only configuration is
.local/github-app-state/broker.json. The command uses noninteractive SSH to run
the broker. It contains connection settings, not GitHub secrets. SSH uses the
host's existing authentication and host-key checking. The broker is a trusted
operator interface, not a public API or an agent RPC. Anyone allowed to run it
has installation authority for the configured owner; keep it outside sandboxes.
The existing sudo-capable SSH account is used for this local demonstration; a
production shared service should have a dedicated restricted service identity.

## Configure a Warden host

Set WARDEN_GITHUB_APP_BROKER to an absolute private JSON configuration file before
starting the control plane or SBX manager. All Engines created in that process
use the same configured source, while requests/grants remain in their separate
state stores. The JSON fields are command (argv array, no shell execution on the
local host), owner (installation account), and app_id. Command selection is
operator-only; no browser/agent endpoint can change it. In App mode the manual
PAT form is hidden and manual-token configuration is rejected.

The current live instance is http://127.0.0.1:18774/ . Existing connected Google
and Figma instances were preserved. Their older running processes were not
restarted or dynamically modified; launch the new combined source with the broker
environment to enable sharing in additional control planes/SBX instances.

No end user creates or pastes a token. Installation/repository selection happens
through GitHub. This deployment enables the existing single owner's installation;
it does not implement authenticated multi-tenant installation discovery. Do not
use one operator's installation authority as another user's personal authority.
Panta's existing personal publication identity flow remains unchanged.

## Permission and refresh behavior

Warden first validates the supported operation, owner/repository, local policy,
and human grant. Only then does the broker look up the repository installation,
check app/owner/suspension, and request a token for exactly that repository and
required permission. Unknown operations fail closed rather than inheriting all
app permissions. The returned repository and permissions are checked.

Supported operations are explicitly listed in host/warden/github_app.py:
repository metadata; selected contents/commit/tree/ref reads and writes; Git
read/push; selected pull-request reads/writes. Issues, account APIs, Actions,
Checks, and other unmapped operations are currently denied. Upstream app permissions
still bound access. Warden's existing write review/approval requirements apply.

Tokens are minted afresh for each approved dispatch, so credentials renew
without user involvement and installation access is rechecked. Tokens are not
cached or persisted. A broker failure denies the request and never falls back
to a PAT. Expiry is checked and Warden rechecks grant time after broker execution.
The process redactor protects the returned token; remote failures return only a
fixed error. The GitHub private key and client secret remain on OVH.

## Validation

Live acceptance used the actual local mitmproxy and existing OVH app store:
unapproved repository read 428, exact-approved panta metadata read 200, replay
428, other repository unapproved 428, unsupported issue creation 403. Caller
credentials were discarded. No repository content, branch, PR, or installation
setting was changed. Tests revoke grants and clear pending requests afterward.
This is not a live SBX containment demonstration. The temporary acceptance proxy
is stopped afterward; the connected control plane remains running.

Unit tests cover RSA JWT signatures and encrypted app-record reuse, permission
narrowing, installation mismatch/suspension, overbroad/malformed response rejection,
long installation token format, source failures, no manual-token fallback,
provider separation, expiry during issuance, and independent grants across Engines.

## Local mode

A local installation does not use the App or the OVH broker. `warden login
github` runs GitHub's OAuth device flow with Warden's own OAuth App client ID
(`release.GitHubOAuthClientID`; a pasted `gh auth token` value is the
fallback) and writes `{"token","login","scopes","obtained"}` to a private
0600 file, by default `<state>/provider/github.json`. `warden-policy
--github-auth-file PATH` selects that file as the credential source; it is
mutually exclusive with `WARDEN_GITHUB_APP_BROKER`. The token is read on every
use, never copied into a guest and never refreshed; a missing or rejected file
fails closed with "Refresh the GitHub sign-in before using repositories".
Injection happens at the same gateway point as installation tokens, only
after a grant matched, with the repository rechecked on each approval; the
repository list comes from `GET /user/repos` (owner, collaborator and
organisation repositories), so there is no owner boundary and no App slug
check. Git over HTTPS uses the same `x-access-token:<token>` basic-auth form:
GitHub ignores the username when a token is the password, and the GitHub CLI
sends that same placeholder. Actions appear as the signed-in person.

## References

- https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app
- https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps (device flow)
- https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens (token as the HTTPS password; username unused)
- https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation
