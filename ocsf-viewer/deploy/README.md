# OVH deployment

Production: https://siem.monaddle.com (the OVH server).

Squarespace DNS: `siem` A record points to the OVH server, TTL 30 minutes.
The apex website and Google Workspace mail records are independent of this app.

Go serves the static React app and authenticated ingestion/query APIs. ClickHouse
stores events; a synced bbolt queue accepts batches during database outages.

## Dependency management

Install Nix and direnv, enable `nix-command flakes`, and add the direnv hook to
your shell. In `ocsf-viewer`, run `direnv allow`, then `npm ci`.
`flake.lock` pins Node, Go, and CLI tools on macOS and Linux; `package-lock.json`
pins JS packages; Dockerfile/Compose pin base images by digest. To work without
an interactive direnv hook: `nix develop --command <command>`.

`npm run dev` provides the React development server. For the production shape:
`npm run build` followed by `npm start` serves the export with Go on port 8080.
`scripts/build.sh` checks React, runs Go race tests/vet, exports the UI, and builds
a static Linux/amd64 Go binary. Then:

```sh
nix develop --command bash scripts/build.sh
docker build --build-arg REVISION=development -t ocsf-explorer:development .
docker run --rm -p 127.0.0.1:8080:8080 ocsf-explorer:development
nix develop --command node scripts/smoke.mjs http://localhost:8080 development
```

## CI/CD

`.github/workflows/ocsf-deploy.yml` runs on changes to this folder and itself.
Pull requests test and build without production secrets. Main pushes and manual
runs on main deploy the exact tested image. Builds run on GitHub-hosted Linux,
not on the VPS. The pipeline uses the pinned Nix shell, `npm ci`, frontend tests,
typechecking/lint, Go race tests/vet, shellcheck, and a real container smoke test
including HTML, revision, JS/CSS assets, and missing-file handling.

The image and Compose configuration are bundled into one immutable artifact
(retained seven days), transferred over pinned-host-key SSH, and loaded on OVH.
This avoids a paid container registry and registry credentials on the server.
The private repository uses the account's GitHub Actions minute/storage quota.

The `ovh-production` GitHub environment contains:

- Secret `OVH_DEPLOY_KEY`: dedicated CI private key, never the personal SSH key.
- Secret `OVH_KNOWN_HOSTS`: host key obtained over the authenticated SSH session.
- Variable `OVH_HOST`: the OVH server address.
- Variable `SITE_HOST`: `siem.monaddle.com`.

The key logs in as `ocsf-ci` with forwarding/PTY disabled and a forced deployment
command. It cannot open an interactive shell. It can deploy trusted Docker and
Compose code, which is a privileged capability; protect the key and main branch.
The receiver is root-owned; pipeline releases cannot silently replace it.

## Server layout and deployment behavior

Run `bootstrap.sh CI_PUBLIC_KEY HOSTNAME` as root once from this directory.
It installs Docker/Compose, the CI account, and `/usr/local/sbin/ocsf-receive`.
Re-run it intentionally when the receiver/bootstrap logic changes; normal CI
updates app images and Compose configuration only.

- `/opt/ocsf/config.env`: persistent, root-readable hostname/runtime configuration.
- `/opt/ocsf/releases/<commit>`: image archive, Compose, Caddyfile, release env.
- `/opt/ocsf/current` and `/opt/ocsf/previous`: successful release pointers.
- Docker volumes `ocsf_caddy_data` and `ocsf_caddy_config`: TLS state.

Only SSH and Caddy's HTTP/HTTPS ports are public. Go listens behind Caddy and on
host loopback port 8080. The Go container runs as a non-root user with a read-only
filesystem, no Linux capabilities, memory/CPU limits, and bounded logs. Caddy
automatically obtains and renews the hostname's public TLS certificate.

Deployments are serialized in GitHub and with a server-side file lock. The
receiver validates the archive and image revision, waits for container health,
and checks the expected revision over public HTTPS before marking it current.
A failure restores the previous image **and its Compose configuration**. The
first-ever failed deployment stops the failed stack. A deploy can briefly
interrupt requests; this is a single-server Compose deployment, not zero downtime.
Docker restart policies recover containers after a host reboot.

## Operations and recovery

```sh
ssh ubuntu@<ovh-host>
sudo docker ps
sudo docker logs --tail 100 ocsf-app-1
curl -fsS https://siem.monaddle.com/healthz
# Roll back to a retained full 40-character commit:
sudo /usr/local/sbin/ocsf-receive 'rollback FULL_COMMIT_SHA'
```

Rollback does not need GitHub or a registry: the image archive is retained on
the server. Releases are retained until explicitly removed. Check disk usage
with `sudo du -sh /opt/ocsf/releases` and `sudo docker system df`; retain current
and previous archives when cleaning old releases. Do not run volume pruning.

For an intentional recovery drill, copy `test-rollback.sh` to the VPS and run it
as root. It creates an image with a failing Docker health check, attempts a real
deployment, verifies that the previous revision returns over HTTPS, and removes
only the synthetic test release. This briefly interrupts the app; run it during
a maintenance window once real users depend on the server.

If delivery fails transiently, use **Re-run failed jobs** in GitHub to reuse the
same tested artifact. Release archives are immutable per commit; a full rebuild
of the same commit can produce different bytes and is intentionally rejected
if that revision already exists. Use a new commit for a rebuilt release.

## Event storage and backups

ClickHouse has no published host ports, a 3 GiB container limit, and a persistent
`ocsf_clickhouse_data` volume. `/opt/ocsf/data/queue.db` is owned by UID 65532.
`config.env` contains distinct admin/read/ingest tokens and the database password.
Run `provision-storage.py` as root to add missing credentials without rotation.
Token rotation is explicit: update config.env and recreate the app container.
Changing the ClickHouse password requires updating the database user as well.

The daily `ocsf-backup.timer` saves a consistent queue snapshot **before** exporting
ClickHouse rows. This preserves every event acknowledged before the snapshot:
queued events can replay with stable IDs even if also present in the event export.
Seven days of compressed backups remain in `/opt/ocsf/backups` (mode 700).
Run `sudo /usr/local/sbin/ocsf-backup` manually; check `systemctl status ocsf-backup.timer`
and `journalctl -u ocsf-backup.service`. A failed backup is visible in systemd;
external alerting and automated off-server copying are not configured.

These copies protect against operator errors, not loss of the VPS. Copy them to
another host or encrypted object storage. Preserve config.env separately and
securely; event backups intentionally exclude credentials. Approximate local
backup RPO is 24 hours if the timer succeeds. No host-loss availability guarantee.

### Restore into an isolated/new deployment

1. Provision the pinned Compose services and credentials; stop the app.
2. Extract the chosen tar into a protected directory; run `sha256sum -c SHA256SUMS`.
3. Start ClickHouse and the app once to create the additive table schema, then stop
   the app again. On a **new empty database**, insert `events.ndjson.gz` with
   `clickhouse-client --query 'INSERT INTO ocsf.events (id,batch_id,time,received_ms,class_uid,severity_id,source,stream,raw) FORMAT JSONEachRow'`.
   Supply credentials securely using the container's environment as backup.sh does.
4. Copy queue.db to the stopped app's data directory, mode 600, owner 65532:65532.
5. Start the app, wait for `/readyz`, and inspect queued/dead-letter counts. Verify
   known records and raw JSON through the authenticated query API. Stable IDs and
   `FINAL` queries collapse duplicates from the queue/export overlap.

Never overwrite a running queue or truncate a production table during recovery.
A code rollback preserves volumes, but a rollback to the old viewer removes the
ClickHouse container and disables ingestion. Restore a pipeline-capable release
before accepting logs again. Future migrations must remain backward-compatible;
image rollback is not a database-schema rollback.

## Google browser sign-in

The login page uses Google Identity Services with a dedicated **SIEM Viewer**
web client in the Monaddle Google Cloud project. Its authorized JavaScript origin
is `https://siem.monaddle.com`; no redirect URI or client secret is needed for
this ID-token flow. Only basic Google identity is requested, with no Gmail API
or offline access. Keep existing clients for other applications separate.

Configure these entries in the root-readable `/opt/ocsf/config.env`:

```dotenv
GOOGLE_CLIENT_ID=YOUR_WEB_CLIENT_ID.apps.googleusercontent.com
OCSF_AUTH_ORIGIN=https://siem.monaddle.com
OCSF_GOOGLE_ADMIN_EMAILS=admin@gmail.com
OCSF_GOOGLE_READ_EMAILS=
OCSF_GOOGLE_READ_DOMAINS=digitalasset.com
```

Both email lists are comma-separated exact addresses. `OCSF_GOOGLE_READ_DOMAINS`
is a comma-separated list of exact domains granting read-only browser access.
The signed Google Workspace `hd` claim must match the email domain; subdomains,
lookalike suffixes, and personal Google accounts with an external email are not
included. Domain grants never grant admin access. Explicit email roles take
precedence, with admin winning if an email is in both email lists. Verified Gmail
addresses and Google Workspace identities with a verified hosted domain are
supported. Missing or partial configuration fails closed. With all five values
absent, browser sign-in shows as unavailable and local files still work.
If the Cloud consent audience is in Testing, the Google account must also be
an allowed test user. The app's email allowlist is always enforced separately.

The Go server validates Google's RSA signature, issuer, client audience,
expiration, subject, verified email, and a one-use ten-minute nonce bound to
an HttpOnly browser cookie. It exchanges that identity for an opaque eight-hour
`__Host-` Secure/HttpOnly/SameSite=Lax cookie. Sessions are held only in server
memory; restarting or deploying the app signs browsers out. Sign out revokes
that session immediately. Cookie-authenticated writes require the exact configured
Origin plus a per-session CSRF header. No Google ID token, API bearer token,
or session credential is stored in browser localStorage.

After allowlist/config changes, recreate the app container through the normal
release pipeline (or the documented operational recreation for configuration
changes). This clears active sessions and applies revocations immediately.
Ingestion, backup, and other machine clients continue using the distinct API
bearer tokens. Those tokens are not accepted by the browser login UI.

Validation follows [Google's ID token verification guidance](https://developers.google.com/identity/gsi/web/guides/verify-google-id-token).
The auth regression suite uses real RSA signatures and a local JWKS endpoint,
including forged/wrong-audience/expired tokens, allowlist denial, CSRF rejection,
nonce replay, session expiry, and logout revocation.
