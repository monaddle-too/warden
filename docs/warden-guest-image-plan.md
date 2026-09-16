# Warden guest image with preinstalled runtimes

Branch `codex/warden-guest-image`, started 2026-09-15 from main 5d008a6.

## Objective

A new sandbox paid about 18 s before its first lease: copying the 324 MB
Codex bundle and the 227 MB Claude executable into the guest and installing
the gateway CA. Owner decision: build one image that contains all of these,
store it on GitHub, commit the way it is made, and make the server use it.

## How it fits SBX

- The runner already creates guests with `sbx create --template REF shell`
  and the policy verifier pins the guest's `image_digest`. A custom image is
  therefore a different template reference plus a different pinned digest;
  no kit or agent change, and `kits` stays empty.
- Probed on OVH: an image built on top of the stock template, saved with
  `docker save` and loaded with `sbx template load`, keeps its manifest
  digest, and `sbx inspect` reports that digest. A `name@sha256:` template
  reference fails to start, so the guest is created by tag and the digest is
  enforced by the verifier.
- The stock template `docker/sandbox-templates:shell-docker` is publicly
  pullable (Ubuntu 26.04, user `agent` with sudo, `/tmp` on the image layer,
  so `/tmp/warden-runtime` and `/tmp/warden-claude` survive stop and start).

## What was built

- `deploy/guest/Dockerfile`: FROM the stock template by digest; fetches the
  official Codex 0.154.0 `codex-package-x86_64-unknown-linux-musl.tar.gz`
  (verified byte for byte against the bundle on OVH: same `bin/codex`,
  code-mode host, rg, bwrap, zsh and `codex-package.json`) and the official
  Claude Code 2.1.272 linux-x64 executable, each by pinned SHA-256; copies
  the public gateway CA (`deploy/guest/warden-proxy.crt`, the certificate
  only) and runs `update-ca-certificates`; writes
  `/opt/warden/guest-manifest.json` naming what it preinstalled.
- `.github/workflows/guest-image.yml`: on push to main touching
  `deploy/guest/**` (or manually), builds for linux/amd64 (and, since the
  two-architecture work below, linux/arm64) and pushes
  `ghcr.io/punished-monaddle/warden-guest:sha-<commit>` and `:latest`,
  printing the digests in the run summary. Uses the repository's own token;
  the package is private like the repository.
- `deploy/guest/install.sh`: on the host, pulls by digest, checks the pulled
  ID equals it, `docker save` into the sandbox runtime with
  `sbx template load`, writes `WARDEN_GUEST_TEMPLATE` and
  `WARDEN_GUEST_DIGEST` into the release `.env`, and recreates policy and
  runner. Existing sandboxes keep their image; new ones use the new one.
- `deploy/chat/compose.yaml`: runner `--template ${WARDEN_GUEST_TEMPLATE}`
  and policy `--guest-image-digest ${WARDEN_GUEST_DIGEST}`, both defaulting
  to the stock template so a release without the variables behaves as before.
- Policy: `SbxCliVerifier.ShellDigest` replaces the constant in the check;
  `warden-policy --guest-image-digest` sets it (validated `sha256:<64 hex>`).
- Runner: the prepare round-trip now reports the guest manifest and whether
  the CA, Codex bundle and Claude executable are present. A manifest whose
  Codex version and target match the host bundle marks the bundle installed;
  a Claude SHA-256 equal to the host executable's (hashed once per host file)
  marks Claude installed; a CA SHA-256 equal to the broker's certificate is
  accepted at stream start without an install. Anything the guest reports is
  treated as untrusted: only exact matches against host files count, and a
  mismatch falls back to the copy. The separate `test -x` round-trip for
  Claude is gone; presence comes from the same report.

## Steps

- [x] Probe SBX template loading and digest reporting on OVH.
- [x] Dockerfile, workflow, install script, compose wiring, verifier flag,
      runner manifest detection, tests; validated the Dockerfile with a real
      build on OVH (3.2 GB image, manifest and binaries as expected).
- [x] Merge to main; the workflow was refused (Actions budget), see below.
- [x] Host: interim server-built copy installed and the services switched.
- [ ] Raise the GitHub Actions budget, re-run the workflow, and move the host
      to the GitHub-published copy with `install.sh` (needs `docker login
      ghcr.io` with `read:packages` because the package is private).
- [ ] Confirm from the runner state that the owner's next new chat used the
      guest image and copied nothing.

## Deployment (2026-09-15)

- Pushing main (b30b0f8) triggered `Warden guest image`, but GitHub refused
  to start the job: "The job was not started because an Actions budget is
  preventing further use." The workflow itself is in place and can be re-run
  once the account's Actions spending limit is raised.
- Interim: the identical committed Dockerfile was built on OVH
  (`warden-guest:local-b30b0f8`, digest `sha256:552a4b5d…c472c1c1`), saved
  and loaded into the sandbox runtime (`sbx template ls` lists it), and a
  throwaway guest created from it reported that digest, the manifest, and
  both runtimes at their paths. Old Warden release images and all Docker
  build cache were pruned first (disk was at 96 percent).
- Release b30b0f8 pinned only the guest digest, which would have failed
  verification for every existing sandbox still on the stock template.
  Release 5073beb (now current) accepts the stock digest beside the pinned
  guest digest; `WARDEN_GUEST_TEMPLATE=warden-guest:local-b30b0f8` and
  `WARDEN_GUEST_DIGEST` are in its `.env`, runner `--template` and policy
  `--guest-image-digest` confirmed on the running containers. Root 200,
  signed-out API 401, edge and SBX active.
- Moving to the GitHub copy later: its digest will differ from the local
  build (different build), so `install.sh IMAGE:TAG sha256:DIGEST` loads it
  and re-pins; sandboxes created from the local image keep working only if
  the local digest is also allowed, so delete those environments first or
  keep `warden-guest:local-b30b0f8` until they are gone.
- Rollback: `ln -sfn /opt/warden/releases/1268343 /opt/warden/current` and
  `docker compose up -d` there (that release ignores the guest variables and
  copies runtimes into every guest as before).

## Two architectures (Track C of the local-deployments plan, 2026-09-15)

Work done on branch `codex/wld-track-c` from a Mac without Docker: the build
files, the verified checksums and the digest evidence. No image was built or
pushed here (the Actions budget is still the blocker); OVH was not touched.

### Pinned runtimes, both architectures

All four files were downloaded from their public releases on 2026-09-15 and
hashed locally; nothing was denied by the network filter.

| Runtime | File | SHA-256 | Cross-check |
|---|---|---|---|
| Codex 0.154.0 amd64 | `codex-package-x86_64-unknown-linux-musl.tar.gz` (126,585,776 bytes) | `fc6e3e3b85f2cf7d664520ee5c66a7fe4aa12bae7d46834f47e2f165fd0d6f78` | equals the release's `codex-package_SHA256SUMS` line and the GitHub API asset `digest`; unchanged from the amd64 pin |
| Codex 0.154.0 arm64 | `codex-package-aarch64-unknown-linux-musl.tar.gz` (118,112,884 bytes) | `97d93e11df72d3c26772db019e6ea8bb72c246500d46b98c760839f3240355e6` | equals `codex-package_SHA256SUMS` and the GitHub API asset `digest` |
| Claude Code 2.1.272 amd64 | `linux-x64/claude` (227,115,320 bytes) | `d81396a668eb76fbddb49a2a5841f1b5d7af96b4c1f6500ced92f2c988f5bcd4` | equals `manifest.json` `platforms.linux-x64.checksum` and size; unchanged from the amd64 pin |
| Claude Code 2.1.272 arm64 | `linux-arm64/claude` (227,074,288 bytes) | `214a90efdd16ee0ea81132ffecced588dba394d178cc494f285ba04b5288c8de` | equals `manifest.json` `platforms.linux-arm64.checksum` and size |

The arm64 Codex bundle has the layout the runner validates
(`codex-package.json` with `layoutVersion` 1, `version` 0.154.0, `target`
`aarch64-unknown-linux-musl`, `entrypoint` `bin/codex`; `bin/codex` and
`bin/codex-code-mode-host` are static aarch64 ELF executables; `codex-path/rg`
and `codex-resources/bwrap` present; `codex-resources/zsh/bin/zsh`). The
Claude file is an aarch64 glibc ELF executable; the manifest also lists
`linux-arm64-musl` and `linux-x64-musl` builds, which Warden does not use
because the guest is Ubuntu (glibc), as before. Both arm64 values are now in
`chat/internal/release/release.go` and in the Dockerfile's per-architecture
build arguments.

### The stock template is a multi-architecture index

Queried Docker Hub directly (token from `auth.docker.io`, manifest GET with
the OCI index and Docker manifest-list Accept headers).
`docker/sandbox-templates@sha256:5fc81bc7…e919` is an
`application/vnd.oci.image.index.v1+json` with four entries:

| Entry | Digest |
|---|---|
| linux/amd64 image manifest (config `sha256:063dc9e6…ee156`, 15 layers, 605 MB compressed) | `sha256:53b08fa716a1725f5a238b69e85f05f96956e621943d7a5c4466502321b07cfd` |
| linux/arm64 image manifest (config `sha256:a60edac9…3ce05`, 15 layers, 589 MB compressed) | `sha256:d353bf15d949bfb5a9de5338cc1285949a9e83d575c185958e449a19d9150190` |
| attestation manifest for amd64 (platform unknown/unknown) | `sha256:20f94f68292efe046bd478cb5a4b77fd42b6020d01c680d9fb0a303b51104434` |
| attestation manifest for arm64 (platform unknown/unknown) | `sha256:997823f3c13c71edc90019878cf76e6d898f42fabf1a6fde24c8e726d371e868` |

The `shell-docker` tag still resolves to that index today, so the existing
`FROM …@sha256:5fc81bc7…` line builds for both platforms: buildx picks the
platform manifest per target. The per-platform digests are recorded in
`release.StockTemplatePlatformDigests`; no separate Apple Silicon template
reference is needed.

### Which digest `sbx inspect` reports

Answer: the index digest, `sha256:5fc81bc7…e919`, on both host types, for a
sandbox the daemon created from the stock reference. Evidence:

- OVH (x86_64 Linux): the verifier pins exactly that value for stock
  sandboxes and passes; if the daemon reported the amd64 manifest
  (`53b08fa7…`) every stock sandbox would fail verification.
- This Apple Silicon Mac: the SBX verifier was written and demonstrated live
  here on 2026-09-11 (commit 47b4dc1, `host/warden/sbx_verifier.py`,
  `SHELL_DIGEST = sha256:5fc81bc7…`) before OVH ran SBX at all, and its
  Warden chat sandboxes (`wc-…`, still recorded under
  `~/Library/Application Support/com.docker.sandboxes/sandboxes/sandboxd/runtimes/`)
  were created with `Template = docker/sandbox-templates:shell-docker@sha256:5fc81bc7…`.
  The daemon's containerd metadata on this Mac stores that image as
  `docker.io/docker/sandbox-templates@sha256:5fc81bc7…` with target media
  type `application/vnd.oci.image.index.v1+json` and digest `5fc81bc7…`; the
  content store holds the index plus both platform manifests. `image_digest`
  is the image target, so it is the index.
- The `sbx` CLI on this Mac could not be asked today: `sbx template ls` and
  `sbx ls` fail with "docker login service unavailable" (the daemon is in the
  state the owner warned about), and no login or daemon start was attempted.

Consequences for pinning:

- Stock template: pin the index digest on both architectures, as now
  (`release.StockTemplateDigest`, `policy.SBXShellDigest`). The verifier's
  allowed set is `release.StockTemplateDigests(GOARCH)`: on amd64 exactly the
  index (unchanged behaviour); on arm64 the index plus the arm64 manifest
  `d353bf15…`, because a stock template that reaches the daemon through
  `sbx template load` of a single-platform tar reports its manifest digest
  (the OVH probe above), and the local installer may load rather than pull.
  Both digests name the same image bytes, so the wider set costs nothing.
- Warden guest image: the workflow now pushes one multi-platform index per
  commit and prints, per platform, the image manifest digest. Pin the
  platform digest (`release.GuestImages[arch].Digest`, `WARDEN_GUEST_DIGEST`,
  `sbx.guestImageDigest`), never the index digest: `install.sh` pulls and
  loads exactly one platform manifest, which is what `sbx inspect` then
  reports, as the single-manifest OVH load already showed.

Confirmation probe (one command sequence, for an operator with a responsive
`sbx` on Apple Silicon; read-only apart from a throwaway sandbox):

```
sbx create --name warden-digest-probe \
  --template docker/sandbox-templates:shell-docker@sha256:5fc81bc7a127e59d81b244a06831ae3212a0310b2e5a0349c54e29249e45e919 \
  --deny-network '**' --no-share-skills shell
sbx inspect warden-digest-probe --json | grep image_digest
sbx rm --force warden-digest-probe
```

Outcomes: `5fc81bc7…` confirms the answer above and nothing changes (the
arm64 set may then be narrowed to the index alone if wanted);
`d353bf15…` means the daemon reports the platform manifest on that host,
which the arm64 set already accepts; record the observation here either way.
After the first guest image install on a Mac, `sbx template ls` must list
the pinned platform digest and a new sandbox's `image_digest` must equal it.

### Build and install changes

- `deploy/guest/Dockerfile`: `ARG TARGETARCH` selects the Codex target
  (`x86_64-unknown-linux-musl` / `aarch64-unknown-linux-musl`), the Claude
  platform (`linux-x64` / `linux-arm64`) and the matching
  `CODEX_PACKAGE_SHA256_<ARCH>` / `CLAUDE_SHA256_<ARCH>` build arguments;
  the Codex `codex-package.json` target is checked, and the manifest is
  written from the installed files (`platform`, Codex version and target,
  Claude SHA-256, CA SHA-256). The runner's manifest parser ignores the new
  `platform` field.
- `.github/workflows/guest-image.yml`: QEMU (`docker/setup-qemu-action`
  v4.4.0, pinned by commit) plus buildx, `platforms: linux/amd64,linux/arm64`,
  no provenance or SBOM manifests, and a summary step that runs
  `docker buildx imagetools inspect` and prints the index digest and a
  per-platform table with the exact `install.sh` line for each.
- `deploy/guest/install.sh`: takes an image manifest digest or the index
  digest (resolved to `--platform`, default this host's), refuses a manifest
  of the wrong platform, pulls by manifest digest, checks the pulled ID,
  loads the tar into the sandbox runtime and pins the digest. New options
  `--platform`, `--compose`, `--env`, `--sbx-home`, `--sbx-user`, `--sbx`
  and `--no-compose` let a local host use its own state root and skip
  Compose; with no options it does exactly what it did on OVH. The env file
  is rewritten portably (BSD sed has no in-place mode without a suffix).

### Remaining

- Raise the Actions budget and run the workflow; copy each platform digest
  into `release.GuestImages` (empty until then).
- Run the confirmation probe on a Mac with a responsive daemon and record
  the outcome.
- `install.sh` needs Docker to pull and `docker save`. A Mac install without
  Docker (the local-deployments installer, Track B) must produce the tar
  itself: fetch the platform manifest, config and layers from GHCR and write
  a `docker save`-layout tar for `sbx template load`, then check that the
  loaded digest equals the pinned platform digest. Whether `sbx template
  load` also accepts an OCI-layout tar has not been tested.

## Building without Docker: snapshot inside a sandbox (2026-09-16)

`scripts/build-guest-image-in-sbx.sh` produces the same guest image on a host
with no Docker and no Actions minutes. It creates a throwaway sandbox from
the stock template in Warden's namespace (deny-all, nothing inside needs
network), copies the host's already-verified runtimes in with `sbx cp` and
places them exactly as warden-runner does (`/tmp/warden-runtime` world-
readable, `/tmp/warden-claude` 0755), installs the gateway CA public
certificate with `update-ca-certificates`, writes
`/opt/warden/guest-manifest.json` in the Dockerfile's format, stops the
sandbox and snapshots it with `sbx template save NAME TAG [--output FILE]`.
The snapshot keeps `/tmp`, so a sandbox created from the tag has the files.

Verified on the Mac (arm64): tag `warden-guest:9a25004-arm64`, digest
`sha256:396434368ab71ec25ad2b4f924703727c0843baf03735aaafe2695351e0c75fc`
as reported by `sbx inspect` of a probe sandbox; the manifest names Codex
0.154.0 aarch64, Claude 2.1.272 and the local gateway CA. Pinned in the
local `warden.json` (`sbx.guestImage`, `sbx.guestImageDigest`). `sbx template
inspect` is cloud-only in 0.42.1; the digest comes from `sbx inspect` of a
sandbox or the `id` column of `template ls --json` (its 12-hex prefix).
Differences from the Dockerfile build: the CA baked in is the local
install's (the runner reinstalls a differing CA anyway), and the image is a
snapshot layer rather than a Dockerfile layer, so its digest is per build;
publish the exported tar with its SHA-256 if it is to be shared.
