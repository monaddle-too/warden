# Warden guest image: the stock Docker Sandboxes shell template with the
# pinned agent runtimes and the Warden gateway CA preinstalled, so the runner
# never copies them into a new sandbox. Built and published for linux/amd64
# and linux/arm64 by .github/workflows/guest-image.yml; installed on a host by
# install.sh.
#
# Everything is fetched from its public release by version and SHA-256, so
# the image is reproducible from this file alone. The paths match what
# warden-runner expects (/tmp/warden-runtime, /tmp/warden-claude) and the
# manifest at /opt/warden/guest-manifest.json tells the runner what is here.
#
# The base reference is the template's multi-architecture OCI index; buildx
# resolves it to the platform manifest for each target (amd64
# sha256:53b08fa716a1725f5a238b69e85f05f96956e621943d7a5c4466502321b07cfd,
# arm64 sha256:d353bf15d949bfb5a9de5338cc1285949a9e83d575c185958e449a19d9150190).
FROM docker/sandbox-templates:shell-docker@sha256:5fc81bc7a127e59d81b244a06831ae3212a0310b2e5a0349c54e29249e45e919

# Set by buildx from --platform: amd64 or arm64. A plain `docker build` sets
# it to the host architecture.
ARG TARGETARCH

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
# Codex: the official "package" bundle (bin/codex, code-mode host, rg, bwrap,
# zsh and codex-package.json), the exact layout the runner validates, for
# this platform's musl target. Claude Code: the official native Linux
# release for this platform. Both are checked against the pinned SHA-256
# before anything is unpacked.
RUN set -eu; \
    case "${TARGETARCH:?TARGETARCH must be amd64 or arm64}" in \
      amd64) CODEX_TARGET=x86_64-unknown-linux-musl;  CODEX_SHA="${CODEX_PACKAGE_SHA256_AMD64}"; CLAUDE_PLATFORM=linux-x64;   CLAUDE_SHA="${CLAUDE_SHA256_AMD64}";; \
      arm64) CODEX_TARGET=aarch64-unknown-linux-musl; CODEX_SHA="${CODEX_PACKAGE_SHA256_ARM64}"; CLAUDE_PLATFORM=linux-arm64; CLAUDE_SHA="${CLAUDE_SHA256_ARM64}";; \
      *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1;; \
    esac; \
    curl -fsSL -o /tmp/codex-package.tgz "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-package-${CODEX_TARGET}.tar.gz"; \
    echo "${CODEX_SHA}  /tmp/codex-package.tgz" | sha256sum -c -; \
    rm -rf /tmp/warden-runtime; mkdir -p /tmp/warden-runtime; \
    tar -xzf /tmp/codex-package.tgz -C /tmp/warden-runtime; rm -f /tmp/codex-package.tgz; \
    grep -q "\"version\": \"${CODEX_VERSION}\"" /tmp/warden-runtime/codex-package.json; \
    grep -q "\"target\": \"${CODEX_TARGET}\"" /tmp/warden-runtime/codex-package.json; \
    test -x /tmp/warden-runtime/bin/codex && test -x /tmp/warden-runtime/bin/codex-code-mode-host; \
    test -x /tmp/warden-runtime/codex-path/rg && test -x /tmp/warden-runtime/codex-resources/bwrap; \
    chmod -R a+rX /tmp/warden-runtime; \
    curl -fsSL -o /tmp/warden-claude "https://storage.googleapis.com/claude-code-dist-86c565f3-f756-42ad-8dfa-d59b1c096819/claude-code-releases/${CLAUDE_VERSION}/${CLAUDE_PLATFORM}/claude"; \
    echo "${CLAUDE_SHA}  /tmp/warden-claude" | sha256sum -c -; \
    chmod 755 /tmp/warden-claude
# Warden gateway CA (public certificate only; rotated copies are reinstalled
# by the runner when their fingerprint differs from this one). The manifest
# records what the layers above installed, read back from the files
# themselves so it cannot disagree with them.
COPY warden-proxy.crt /usr/local/share/ca-certificates/warden-proxy.crt
RUN set -eu; chmod 644 /usr/local/share/ca-certificates/warden-proxy.crt; update-ca-certificates >/dev/null; \
    mkdir -p /opt/warden; \
    CODEX_TARGET="$(sed -n 's/.*"target": *"\([^"]*\)".*/\1/p' /tmp/warden-runtime/codex-package.json)"; \
    test -n "${CODEX_TARGET}"; \
    printf '{"platform":"linux/%s","codex":{"version":"%s","target":"%s"},"claude":{"version":"%s","sha256":"%s"},"ca":{"sha256":"%s"}}\n' \
      "${TARGETARCH}" "${CODEX_VERSION}" "${CODEX_TARGET}" "${CLAUDE_VERSION}" "$(sha256sum /tmp/warden-claude | cut -d' ' -f1)" "$(sha256sum /usr/local/share/ca-certificates/warden-proxy.crt | cut -d' ' -f1)" \
      > /opt/warden/guest-manifest.json; \
    chmod 644 /opt/warden/guest-manifest.json; cat /opt/warden/guest-manifest.json
USER agent
