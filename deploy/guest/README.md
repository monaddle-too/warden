# Warden guest images

Two images carry the same pinned agent runtimes (Codex bundle and Claude
executable, fetched by version and SHA-256 with `fetch-runtimes.sh`) and the
Warden gateway CA; they differ in what they sit on and where things are.
The release test in `chat/internal/release` (`TestGuestDockerfilesPinReleaseRuntimes`)
keeps both Dockerfiles' pinned `ARG`s equal to each other and to the runner's
constants.

| | `warden-guest-base` (`Dockerfile.base`) | `warden-guest` (`Dockerfile`) |
|---|---|---|
| For | Kubernetes sandboxes (plan decision 7) | SBX sandboxes (`install.sh`) |
| Base | `ubuntu:24.04`, pinned by digest | `docker/sandbox-templates:shell-docker`, pinned by digest |
| User | `agent`, uid/gid 1000, home `/home/agent`, passwordless sudo | the template's `agent` user |
| Codex bundle | `/opt/warden/runtime` | `/tmp/warden-runtime` |
| Claude executable | `/opt/warden/claude/claude` | `/tmp/warden-claude` |
| Trust | `/opt/warden/trust/ca-certificates.crt` (mountable, see below) | `/etc/ssl/certs/ca-certificates.crt`, kept by the runner's CA install |
| Manifest `variant` | `base` | `sbx` |
| Persisted across stop | `/home/agent` only | the whole root filesystem (SBX) |

Both write `/opt/warden/guest-manifest.json` with `write-manifest.sh`, reading
every value back from the installed files:

```json
{"platform":"linux/arm64","variant":"base",
 "codex":{"version":"0.154.0","target":"aarch64-unknown-linux-musl"},
 "claude":{"version":"2.1.272","sha256":"<sha256 of the executable>"},
 "ca":{"sha256":"<sha256 of warden-proxy.crt>"},
 "paths":{"codex":"/opt/warden/runtime","claude":"/opt/warden/claude/claude",
          "trust":"/opt/warden/trust/ca-certificates.crt","home":"/home/agent"},
 "user":{"name":"agent","uid":1000,"gid":1000}}
```

`platform`, `codex`, `claude` and `ca` are what the runner reads today;
`variant`, `paths` and `user` (the owner of the home, the account exec
sessions run as) are new and let a driver stop assuming the `/tmp` paths
and uid 1000. The runner ignores a manifest over 4096 bytes.

## Trust mount contract (base image)

The base image runs `update-ca-certificates` with `warden-proxy.crt` (the
public gateway CA committed here) installed, moves the resulting bundle to
`/opt/warden/trust/ca-certificates.crt` and makes
`/etc/ssl/certs/ca-certificates.crt` a symlink to it. The environment
(`SSL_CERT_FILE`, `SSL_CERT_DIR=/opt/warden/trust`, `NODE_EXTRA_CA_CERTS`,
`REQUESTS_CA_BUNDLE`, `PIP_CERT`, `GIT_SSL_CAINFO`, `CURL_CA_BUNDLE`) points
at the same file for clients that ignore the system path.

- Mount a directory at `/opt/warden/trust` containing `ca-certificates.crt`
  (the policy service's ConfigMap: the system CAs plus the gateway CA in
  force, decision 9) and every TLS client in the guest verifies against it.
  A ConfigMap mounted as a directory is refreshed in place by the kubelet,
  so a rotation reaches running guests without exec, root or restart.
- Mount nothing and the guest uses the bundle it was built with, which
  trusts the CA in `warden-proxy.crt`.
- The mounted file must be a complete bundle (public CAs included), not the
  gateway CA alone, because it replaces the system bundle.
- `update-ca-certificates` run inside a guest replaces the symlink with a
  regenerated file; the environment still points at the mount, and image
  state does not persist anyway.

## Persisted set (base image)

`/home/agent` is the only path that survives a stop: the Kubernetes driver
mounts the per-sandbox PersistentVolumeClaim there (decision 8). Package
installs, `/tmp` (an emptyDir) and everything else are image state and come
back fresh with the next pod. The entrypoint (`tini` then
`/opt/warden/bin/guest-init`) takes ownership of an empty, root-owned volume
and seeds it from `/etc/skel`; a pod spec should set `args`, not `command`,
so both stay in place. The default command is `sleep infinity`.

## Building

- CI: `.github/workflows/guest-image.yml` builds and pushes both images for
  linux/amd64 and linux/arm64 (`ghcr.io/<owner>/warden-guest` and
  `ghcr.io/<owner>/warden-guest-base`, tags `sha-<commit>` and `latest`) and
  lists each platform's image manifest digest in the run summary. Pin a
  platform digest, never the index digest.
- Base image locally, or straight into a k3s node's containerd so no
  registry is needed:

  ```sh
  deploy/guest/build-base.sh --platform linux/arm64          # docker or nerdctl
  deploy/guest/build-base.sh --k3s                            # nerdctl --namespace k8s.io on the node
  ```

  It tags `warden-guest-base:<git describe>` and prints the image ID and
  the manifest digest to pin as `kubernetes.guestImageDigest`.
- SBX variant without Docker: `scripts/build-guest-image-in-sbx.sh`
  snapshots a throwaway sandbox with the host's verified runtimes and pins
  the result in `warden.json`. It cannot make the base image (a snapshot of
  an SBX template is not an Ubuntu image in a container runtime's store).
- Either Dockerfile builds on its own with BuildKit (`docker build`,
  `docker buildx build`, `nerdctl build`); the scripts here are bind-mounted
  into the build and are not left in the image, except `guest-init.sh`.
