# Repository workflow

Warden now supports ordinary Git clone, fetch, and reviewed single-branch push
over HTTPS, plus a guest CLI that carries a change through dependency setup,
checks, local review, publication, and a draft pull request. GitHub credentials
remain on the host and trusted Linux appliance. No guest `gh auth login` is needed.

## Start in an existing guest

The helper is included in provisioning and offline staging. To add it to an
already running guest without restarting it, run in the **guest Terminal**:

```sh
curl -fsS http://10.77.0.1:8081/warden-repo -o /tmp/warden-repo
sudo install -m 755 /tmp/warden-repo /usr/local/bin/warden-repo
warden-repo doctor
```

Use the existing `warden-trust-proxy` setup if the proxy CA is not yet trusted.
Apple Command Line Tools provide Git and compilers. Node/npm, gh, and Homebrew
are provisioned separately. Do not disable TLS verification to resolve CA errors.

```sh
warden-repo clone OWNER/REPO
cd REPO
warden-repo branch improve-validation
warden-repo setup
# Edit files and commit normally inside the guest.
git add src tests
git commit -m "Validate input before saving"
warden-repo check
warden-repo review
warden-repo publish
warden-repo pr --base main --title "Validate input before saving"
```

The first clone returns HTTP 428 until the host user approves a **repository read
session**. The CLI waits and retries every four seconds, for up to ten minutes.
Use `--wait 0` for immediate return or `--wait 1800` for a longer wait. A read
session covers ref discovery and upload-pack for exactly one repository for up to
an hour. It does not authorize a push or REST mutations. Revoke it in the host
dashboard's Active permissions view. Restart and policy changes revoke grants.

Normal Git also works: `git clone https://github.com/OWNER/REPO.git`, `git fetch`,
and `git push --no-thin origin HEAD:refs/heads/BRANCH`. Normal Git does not wait
for host approval; rerun after approval. Use `.git` HTTPS remotes, not SSH aliases.
Avoid simultaneous pushes: an exact approval authorizes one request once.

## Dependencies and project checks

For npm projects, `setup` runs `npm ci --ignore-scripts --no-audit --no-fund` using
the committed package lock. Lifecycle scripts require explicit `setup --scripts`.
For Python, commit a complete hashed `requirements.lock`; setup creates `.venv`
and installs with `--require-hashes --only-binary=:all: --no-deps`. The lock must
include every dependency. Source builds and unpinned transitive dependencies are
intentionally not implicit. Ignore `.venv` and `node_modules` in the project.

Public npm/Python registry downloads use inspected HTTPS. GitHub Git dependencies
need a read session for each repository. GitHub release assets, GHCR, LFS and
arbitrary GitHub archives remain blocked; many Homebrew formulae will still need
a future artifact-download policy. A missing package is not solved by disabling
Warden. pnpm, Yarn, Cargo, Go and other builds can use explicit guest commands and
custom checks; their installation is not inferred or silently changed.

`check` detects npm scripts named lint, typecheck, test, and build and runs the
ones present with `CI=1`. Python defaults to `.venv/bin/python -m pytest`. For a
different project, run `warden-repo configure` and review/commit its configuration:

```json
{
  "version": 1,
  "setup": "npm",
  "checks": [
    ["npm", "run", "lint"],
    ["npm", "test", "--", "--runInBand"],
    ["npm", "run", "build"]
  ]
}
```

Use `"setup": "none"` for a manually managed environment. Check commands are
argument arrays, executed directly without a shell. They are untrusted project
code and run only inside the guest. No test command is executed on the host or
Linux appliance. The CLI requires a clean working tree and records passing
checks for the exact commit in Git's private directory. A new commit requires
new checks. These receipts are convenience checks, **not attestations**: guest
root can forge them or run ordinary Git. The host independently enforces push
approval regardless of the guest CLI or receipt.

## What a push approval means

1. The proxy parses Git's packet framing and permits one SHA-1 branch update.
   Deletion, tags, extra refs, push options, signed push certificates, alternate
   object formats, and malformed frames fail closed.
2. An active repository read session permits fetching its upstream objects into
   a temporary bare repository in the Linux appliance's `/dev/shm`. Git connects
   only to independently resolved/pinned github.com, verifies TLS, and follows no
   redirects. Host credentials are passed in the child environment, never argv.
3. Git verifies objects/connectivity, checks the old branch tip and fast-forward
   ancestry, and generates patches for **every outgoing commit**, including merge
   history. Binary changes anywhere in that history and submodule updates fail
   closed. A branch's net diff alone cannot hide a secret committed and reverted.
4. Warden regenerates the outgoing pack from reachable objects only. Extra
   unreferenced objects supplied by the guest are discarded before publication.
5. The host presents the repository, branch, old/new object IDs, base, net diff
   statistics, and outgoing commit patches. Approval binds the complete resulting
   HTTP request including the canonical pack bytes, and is single use. Changing
   the content or headers requires another approval. Force pushes remain blocked.
6. Permission is checked before dispatch and before releasing the response.
   GitHub independently checks the old ref value and its branch protections.
   Revocation cannot undo a request GitHub already received.

Initial pushes to empty repositories are supported. On existing repositories,
new branches are reviewed against their fork point from the default branch.
Updates to existing branches are checked against that branch's current tip.

Source previews and pull-request fields are held in bounded host memory for ten
minutes and returned only through the authenticated host API. Linux caches up to
four canonical packs/previews for two minutes to support exact retries; temporary
repositories are removed after inspection. No source preview is written into
SQLite, JSONL, or the SIEM. Audit retains operation, repository, byte counts and
ref/object identifiers. This is application retention, not protection from OS
swap, VM snapshots, privileged debugging or crash dumps.

Limits: 8 MiB request/canonical push pack; 128 MiB Git response; 256 MiB aggregate
review scratch per job; 128 MiB per scratch file; 256 KiB per review output; two
concurrent review jobs; 90 seconds per Git subprocess plus CPU/address-space
limits. Other inspected responses remain limited to 16 MiB on delivery (the
shared proxy may buffer up to 128 MiB before rejecting an unknown-length body).
Large repositories/changes fail closed; split the change or extend the limits
with deliberate resource validation. Git is a trusted parser inside Linux; these
limits are not a sandbox proof against a Git vulnerability.

## Pull requests and recovery

`pr` first verifies that the remote branch equals local HEAD, then checks for an
existing open PR through REST. Both the list and create operation require their
own host approvals. Creation is always a draft. The host review shows the exact
head, base, title and description; source/body fields remain out of persistent
audit storage. `--body-file PATH` reads a description from a guest file.

Only an explicit HTTP 428 triggers automatic retry. An ambiguous network error
never causes an automatic write replay. Check `git ls-remote` or `warden-repo
fetch` after a failed push; rerunning Git renegotiates the current ref. Rerunning
`pr` checks for an existing PR before creation. If the branch moved, fetch and
rebase, rerun checks, and request a fresh push approval. Nothing automatically
resets, force-pushes, deletes, stashes, or discards working files.

The working repository and check receipt persist in the guest disk. They are not
an off-machine backup. Publish reviewed work regularly. Automatic guest disk
snapshots, protected branch merges, and conflict resolution remain explicit work.

## Scope and validation

This gates **direct GitHub traffic**. Ordinary public HTTPS is still allowed;
OpenAI-hosted search, connectors and other remote relays can access GitHub outside
this gate. This implementation does not claim to prevent source exfiltration to
allowed services, forge-proof guest test results, or every indirect GitHub write.

`tests/test_repository.py` drives a stock Git client through real Guard and Engine
code into a local Git HTTP backend. It exercises clone, fetch, first push, review,
approved publish, changed-content rejection, force-push rejection, expired review,
per-repository scope, hidden binary history and removal of unreachable objects.
`tests/test_repository_cli.py` checks actual check receipts, failed checks,
lifecycle-script defaults and preservation of existing directories. Tests use
temporary repositories and test-only credentials; they do not publish to GitHub.

Protocol references: [Git HTTP protocol](https://git-scm.com/docs/gitprotocol-http),
[pack protocol](https://git-scm.com/docs/gitprotocol-pack),
[capabilities](https://git-scm.com/docs/gitprotocol-capabilities), and
[Git HTTP configuration](https://git-scm.com/docs/git-config).
