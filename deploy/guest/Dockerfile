# Warden guest image, SBX variant (warden-guest): the stock Docker Sandboxes
# shell template with the pinned agent runtimes and the Warden gateway CA
# preinstalled, so the runner never copies them into a new sandbox. Built
# and published for linux/amd64 and linux/arm64 by
# .github/workflows/guest-image.yml; installed on a host by install.sh.
# Kubernetes sandboxes use Dockerfile.base (warden-guest-base) instead,
# which puts the same runtimes under /opt/warden; the two files share the
# fetch and manifest scripts and the release test in chat/internal/release
# keeps their pinned ARGs equal.
#
# Everything is fetched from its public release by version and SHA-256
# (fetch-runtimes.sh), so the image is reproducible from this directory
# alone. The paths match what warden-runner expects today
# (/tmp/warden-runtime, /tmp/warden-claude; the template keeps /tmp on the
# image layer, so they survive stop and start) and the manifest at
# /opt/warden/guest-manifest.json tells the runner what is here and where.
# Trust in this variant stays the system bundle that the runner's CA install
# (`sbx cp` plus `update-ca-certificates`) maintains; there is no
# /opt/warden/trust mount in SBX.
#
# BuildKit is required (RUN --mount): docker build, docker buildx and
# nerdctl build all use it.
#
# The base reference is the template's multi-architecture OCI index; buildx
# resolves it to the platform manifest for each target (amd64
# sha256:53b08fa716a1725f5a238b69e85f05f96956e621943d7a5c4466502321b07cfd,
# arm64 sha256:d353bf15d949bfb5a9de5338cc1285949a9e83d575c185958e449a19d9150190).
FROM docker/sandbox-templates:shell-docker@sha256:5fc81bc7a127e59d81b244a06831ae3212a0310b2e5a0349c54e29249e45e919

# Set by buildx from --platform: amd64 or arm64. A plain `docker build` sets
# it to the host architecture.
ARG TARGETARCH

# Pinned runtimes. Keep these equal to Dockerfile.base and to the constants
# in chat/internal/release/release.go (TestGuestDockerfilesPinReleaseRuntimes).
ARG CODEX_VERSION=0.154.0
# codex-package-<target>.tar.gz from the rust-v${CODEX_VERSION} release;
# both match the release's codex-package_SHA256SUMS.
ARG CODEX_PACKAGE_SHA256_AMD64=fc6e3e3b85f2cf7d664520ee5c66a7fe4aa12bae7d46834f47e2f165fd0d6f78
ARG CODEX_PACKAGE_SHA256_ARM64=97d93e11df72d3c26772db019e6ea8bb72c246500d46b98c760839f3240355e6
ARG CLAUDE_VERSION=2.1.272
# linux-x64 and linux-arm64 from the release's manifest.json (glibc builds).
ARG CLAUDE_SHA256_AMD64=d81396a668eb76fbddb49a2a5841f1b5d7af96b4c1f6500ced92f2c988f5bcd4
ARG CLAUDE_SHA256_ARM64=214a90efdd16ee0ea81132ffecced588dba394d178cc494f285ba04b5288c8de

USER root
# The native cairo library (libcairo2 with its headers and pkg-config
# file), which the template does not ship: charting and image tools that
# load or link cairo (cairocffi, pycairo, node-canvas, rsvg, matplotlib's
# cairo backends) then work without a package install. Dockerfile.base
# installs the same for Kubernetes guests.
RUN set -eu; export DEBIAN_FRONTEND=noninteractive; \
    apt-get update; \
    apt-get install -y --no-install-recommends libcairo2 libcairo2-dev pkg-config; \
    rm -rf /var/lib/apt/lists/*
# The pinned runtimes at the paths the SBX runner expects, fetched and
# verified by the shared script (see fetch-runtimes.sh for what it checks).
RUN --mount=type=bind,source=fetch-runtimes.sh,target=/tmp/fetch-runtimes.sh \
    sh /tmp/fetch-runtimes.sh /tmp/warden-runtime /tmp/warden-claude
# Warden gateway CA (public certificate only; rotated copies are reinstalled
# by the runner when their fingerprint differs from this one), then the
# manifest, read back from the files themselves so it cannot disagree with
# them (see write-manifest.sh for the schema).
COPY warden-proxy.crt /usr/local/share/ca-certificates/warden-proxy.crt
RUN --mount=type=bind,source=write-manifest.sh,target=/tmp/write-manifest.sh \
    set -eu; \
    chmod 644 /usr/local/share/ca-certificates/warden-proxy.crt; \
    update-ca-certificates >/dev/null; \
    sh /tmp/write-manifest.sh sbx /tmp/warden-runtime /tmp/warden-claude \
      /etc/ssl/certs/ca-certificates.crt /home/agent \
      /usr/local/share/ca-certificates/warden-proxy.crt /opt/warden/guest-manifest.json
USER agent
