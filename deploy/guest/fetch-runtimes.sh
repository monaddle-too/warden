#!/bin/sh
# Fetch the pinned agent runtimes into a guest image build. Shared by
# Dockerfile.base and Dockerfile, which bind-mount this file into one RUN
# step so the two variants cannot drift in how they fetch and verify.
#
#   fetch-runtimes.sh CODEX_DIR CLAUDE_FILE
#
# Reads from the environment (the Dockerfiles' ARGs, which the release test
# in chat/internal/release keeps equal to the constants the runner uses):
#   TARGETARCH                   amd64 or arm64 (set by buildx from --platform)
#   CODEX_VERSION                the rust-v<version> Codex release
#   CODEX_PACKAGE_SHA256_AMD64   codex-package-x86_64-unknown-linux-musl.tar.gz
#   CODEX_PACKAGE_SHA256_ARM64   codex-package-aarch64-unknown-linux-musl.tar.gz
#   CLAUDE_VERSION               the Claude Code release
#   CLAUDE_SHA256_AMD64          the linux-x64 glibc executable
#   CLAUDE_SHA256_ARM64          the linux-arm64 glibc executable
#
# Codex: the official "package" bundle (bin/codex, the code-mode host, rg,
# bwrap, zsh and codex-package.json), the exact layout the runner validates,
# for this platform's musl target. Claude Code: the official native Linux
# release for this platform. Both are checked against the pinned SHA-256
# before anything is unpacked, and the unpacked bundle is checked to be the
# pinned version and target with every executable the runner requires.
set -eu
[ $# -eq 2 ] || { echo "usage: fetch-runtimes.sh CODEX_DIR CLAUDE_FILE" >&2; exit 2; }
CODEX_DIR="$1"; CLAUDE_FILE="$2"
for v in TARGETARCH CODEX_VERSION CODEX_PACKAGE_SHA256_AMD64 CODEX_PACKAGE_SHA256_ARM64 CLAUDE_VERSION CLAUDE_SHA256_AMD64 CLAUDE_SHA256_ARM64; do
  eval "value=\${$v:-}"
  [ -n "$value" ] || { echo "fetch-runtimes.sh: $v is not set" >&2; exit 2; }
done
case "$TARGETARCH" in
  amd64) CODEX_TARGET=x86_64-unknown-linux-musl;  CODEX_SHA="$CODEX_PACKAGE_SHA256_AMD64"; CLAUDE_PLATFORM=linux-x64;   CLAUDE_SHA="$CLAUDE_SHA256_AMD64";;
  arm64) CODEX_TARGET=aarch64-unknown-linux-musl; CODEX_SHA="$CODEX_PACKAGE_SHA256_ARM64"; CLAUDE_PLATFORM=linux-arm64; CLAUDE_SHA="$CLAUDE_SHA256_ARM64";;
  *) echo "fetch-runtimes.sh: unsupported TARGETARCH $TARGETARCH (amd64 or arm64)" >&2; exit 1;;
esac

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "fetching Codex ${CODEX_VERSION} (${CODEX_TARGET}) into ${CODEX_DIR}"
curl -fsSL -o "$WORK/codex-package.tgz" "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-package-${CODEX_TARGET}.tar.gz"
echo "${CODEX_SHA}  $WORK/codex-package.tgz" | sha256sum -c -
rm -rf -- "$CODEX_DIR"
mkdir -p "$CODEX_DIR"
tar -xzf "$WORK/codex-package.tgz" -C "$CODEX_DIR"
grep -q "\"version\": \"${CODEX_VERSION}\"" "$CODEX_DIR/codex-package.json"
grep -q "\"target\": \"${CODEX_TARGET}\"" "$CODEX_DIR/codex-package.json"
for exe in bin/codex bin/codex-code-mode-host codex-path/rg codex-resources/bwrap; do
  test -x "$CODEX_DIR/$exe" || { echo "fetch-runtimes.sh: $exe is not executable in the Codex bundle" >&2; exit 1; }
done
chmod -R a+rX "$CODEX_DIR"

echo "fetching Claude Code ${CLAUDE_VERSION} (${CLAUDE_PLATFORM}) into ${CLAUDE_FILE}"
mkdir -p "$(dirname "$CLAUDE_FILE")"
curl -fsSL -o "$WORK/claude" "https://storage.googleapis.com/claude-code-dist-86c565f3-f756-42ad-8dfa-d59b1c096819/claude-code-releases/${CLAUDE_VERSION}/${CLAUDE_PLATFORM}/claude"
echo "${CLAUDE_SHA}  $WORK/claude" | sha256sum -c -
install -m 755 "$WORK/claude" "$CLAUDE_FILE"
