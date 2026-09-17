#!/bin/sh
# Write /opt/warden/guest-manifest.json, the file the runner reads to learn
# what a guest image preinstalled and where. Shared by Dockerfile.base and
# Dockerfile (bind-mounted into their manifest RUN step).
#
#   write-manifest.sh VARIANT CODEX_DIR CLAUDE_FILE TRUST_FILE HOME_DIR CA_FILE OUTPUT
#
#   VARIANT      base (plain image, Kubernetes) or sbx (Docker Sandboxes template)
#   CODEX_DIR    the unpacked Codex bundle (contains codex-package.json)
#   CLAUDE_FILE  the Claude executable
#   TRUST_FILE   the CA bundle TLS clients in the guest verify against
#   HOME_DIR     the agent home, the only path a Kubernetes sandbox persists
#   CA_FILE      the Warden gateway CA the image was built with (public cert)
#   OUTPUT       the manifest path
#
# Reads TARGETARCH, CODEX_VERSION and CLAUDE_VERSION from the environment.
# Every value in the manifest is read back from the files themselves (the
# Codex version and target from codex-package.json, the Claude and CA digests
# by hashing), so the manifest cannot disagree with what the layers hold;
# the pinned versions are only checked against what was read.
#
# Schema (one line; the runner ignores a manifest over 4096 bytes):
#   {"platform":"linux/<arch>","variant":"base|sbx",
#    "codex":{"version":..,"target":..},"claude":{"version":..,"sha256":..},
#    "ca":{"sha256":..},
#    "paths":{"codex":CODEX_DIR,"claude":CLAUDE_FILE,"trust":TRUST_FILE,"home":HOME_DIR}}
set -eu
[ $# -eq 7 ] || { echo "usage: write-manifest.sh VARIANT CODEX_DIR CLAUDE_FILE TRUST_FILE HOME_DIR CA_FILE OUTPUT" >&2; exit 2; }
VARIANT="$1"; CODEX_DIR="$2"; CLAUDE_FILE="$3"; TRUST_FILE="$4"; HOME_DIR="$5"; CA_FILE="$6"; OUTPUT="$7"
case "$VARIANT" in base|sbx) ;; *) echo "write-manifest.sh: variant must be base or sbx" >&2; exit 2;; esac
case "${TARGETARCH:-}" in amd64|arm64) ;; *) echo "write-manifest.sh: TARGETARCH must be amd64 or arm64" >&2; exit 2;; esac
[ -n "${CODEX_VERSION:-}" ] && [ -n "${CLAUDE_VERSION:-}" ] || { echo "write-manifest.sh: CODEX_VERSION and CLAUDE_VERSION must be set" >&2; exit 2; }
for p in "$CODEX_DIR" "$CLAUDE_FILE" "$TRUST_FILE" "$HOME_DIR" "$CA_FILE"; do
  case "$p" in /*) ;; *) echo "write-manifest.sh: $p is not an absolute path" >&2; exit 2;; esac
  case "$p" in *[\"\\]*) echo "write-manifest.sh: $p contains a quote or backslash" >&2; exit 2;; esac
done
test -x "$CODEX_DIR/bin/codex" || { echo "write-manifest.sh: $CODEX_DIR/bin/codex is not executable" >&2; exit 1; }
test -x "$CLAUDE_FILE" || { echo "write-manifest.sh: $CLAUDE_FILE is not executable" >&2; exit 1; }
test -r "$TRUST_FILE" || { echo "write-manifest.sh: $TRUST_FILE is not readable" >&2; exit 1; }
test -d "$HOME_DIR" || { echo "write-manifest.sh: $HOME_DIR is not a directory" >&2; exit 1; }
grep -q 'BEGIN CERTIFICATE' "$CA_FILE" || { echo "write-manifest.sh: $CA_FILE is not a PEM certificate" >&2; exit 1; }
grep -q 'PRIVATE KEY' "$CA_FILE" && { echo "write-manifest.sh: $CA_FILE contains a private key" >&2; exit 1; }
grep -qF -- "$(sed -n '/BEGIN CERTIFICATE/{n;p;q;}' "$CA_FILE")" "$TRUST_FILE" || { echo "write-manifest.sh: $TRUST_FILE does not contain $CA_FILE" >&2; exit 1; }

CODEX_TARGET="$(sed -n 's/.*"target": *"\([^"]*\)".*/\1/p' "$CODEX_DIR/codex-package.json")"
CODEX_FOUND="$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' "$CODEX_DIR/codex-package.json")"
[ -n "$CODEX_TARGET" ] && [ -n "$CODEX_FOUND" ] || { echo "write-manifest.sh: $CODEX_DIR/codex-package.json has no version or target" >&2; exit 1; }
[ "$CODEX_FOUND" = "$CODEX_VERSION" ] || { echo "write-manifest.sh: bundle is Codex $CODEX_FOUND, pinned $CODEX_VERSION" >&2; exit 1; }
case "$TARGETARCH:$CODEX_TARGET" in amd64:x86_64-*|arm64:aarch64-*) ;; *) echo "write-manifest.sh: Codex target $CODEX_TARGET is not for $TARGETARCH" >&2; exit 1;; esac
CLAUDE_SHA="$(sha256sum "$CLAUDE_FILE" | cut -d' ' -f1)"
CA_SHA="$(sha256sum "$CA_FILE" | cut -d' ' -f1)"

mkdir -p "$(dirname "$OUTPUT")"
printf '{"platform":"linux/%s","variant":"%s","codex":{"version":"%s","target":"%s"},"claude":{"version":"%s","sha256":"%s"},"ca":{"sha256":"%s"},"paths":{"codex":"%s","claude":"%s","trust":"%s","home":"%s"}}\n' \
  "$TARGETARCH" "$VARIANT" "$CODEX_FOUND" "$CODEX_TARGET" "$CLAUDE_VERSION" "$CLAUDE_SHA" "$CA_SHA" \
  "$CODEX_DIR" "$CLAUDE_FILE" "$TRUST_FILE" "$HOME_DIR" > "$OUTPUT"
chmod 644 "$OUTPUT"
[ "$(wc -c < "$OUTPUT")" -le 4096 ] || { echo "write-manifest.sh: manifest exceeds 4096 bytes" >&2; exit 1; }
cat "$OUTPUT"
