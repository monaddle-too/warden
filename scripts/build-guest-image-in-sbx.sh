#!/bin/sh
# Build the Warden guest image inside an sbx sandbox, without Docker.
#
# The guest image is the stock shell template with the pinned Codex bundle,
# the Claude executable and the gateway CA preinstalled (what
# deploy/guest/Dockerfile builds). On a host without Docker, or without
# GitHub Actions minutes, the same image is made by creating a throwaway
# sandbox from the stock template in Warden's namespace, copying the
# already-verified runtimes in from the host exactly as warden-runner does,
# and snapshotting it with `sbx template save`. Nothing inside the sandbox
# needs network; the sandbox is created deny-all.
#
#   scripts/build-guest-image-in-sbx.sh [--state DIR] [--tag TAG] [--ca FILE] [--output FILE]
#
# Defaults: state ~/.warden (its bin/warden-sbx wrapper, runtimes/ and the
# policy gateway CA); tag warden-guest:<git revision>-<guest arch>; the CA
# is the local install's public gateway certificate so its guests skip the
# per-sandbox CA install. Prints the template digest to pin as
# sbx.guestImageDigest and the tag for sbx.guestImage in warden.json.
set -eu
cd "$(dirname "$0")/.."

STATE="$HOME/.warden"
TAG=""
CA=""
OUTPUT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --state) STATE="$2"; shift 2 ;;
    --tag) TAG="$2"; shift 2 ;;
    --ca) CA="$2"; shift 2 ;;
    --output) OUTPUT="$2"; shift 2 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
SBX="$STATE/bin/warden-sbx"
RUNTIMES="$STATE/runtimes"
[ -x "$SBX" ] || { echo "$SBX missing; run warden install first" >&2; exit 1; }
[ -f "$RUNTIMES/codex/codex-package.json" ] || { echo "$RUNTIMES/codex is not an installed Codex bundle" >&2; exit 1; }
[ -x "$RUNTIMES/claude/claude" ] || { echo "$RUNTIMES/claude/claude missing" >&2; exit 1; }
[ -n "$CA" ] || CA="$STATE/policy/gateway-ca/mitmproxy-ca-cert.pem"
[ -f "$CA" ] || { echo "gateway CA certificate $CA missing (start Warden once, or pass --ca deploy/guest/warden-proxy.crt)" >&2; exit 1; }
grep -q 'BEGIN CERTIFICATE' "$CA" || { echo "$CA is not a PEM certificate" >&2; exit 1; }
grep -q 'PRIVATE KEY' "$CA" && { echo "$CA contains a private key; pass the public certificate only" >&2; exit 1; }

CODEX_TARGET="$(sed -n 's/.*"target": *"\([^"]*\)".*/\1/p' "$RUNTIMES/codex/codex-package.json")"
CODEX_VERSION="$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' "$RUNTIMES/codex/codex-package.json")"
case "$CODEX_TARGET" in
  aarch64-*) ARCH=arm64 ;;
  x86_64-*) ARCH=amd64 ;;
  *) echo "unknown Codex target $CODEX_TARGET" >&2; exit 1 ;;
esac
CLAUDE_VERSION="$(grep -o 'ClaudeVersion = "[^"]*"' chat/internal/release/release.go | sed 's/.*"\(.*\)"/\1/')"
STOCK="$(grep -o 'StockTemplate *= "[^"]*"' chat/internal/release/release.go | sed 's/.*"\(.*\)"/\1/')"
[ -n "$TAG" ] || TAG="warden-guest:$(git rev-parse --short HEAD)-$ARCH"
NAME="warden-guest-build-$(date +%s)"
echo "building $TAG ($ARCH, Codex $CODEX_VERSION $CODEX_TARGET, Claude $CLAUDE_VERSION) from $STOCK in $STATE"

cleanup() { "$SBX" rm --force "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# 1. A throwaway sandbox from the stock template, no network, no shared skills.
"$SBX" create --name "$NAME" --cpus 1 --memory 2048m --template "$STOCK" --deny-network '**' --no-share-skills shell >/dev/null
guest_arch="$("$SBX" exec "$NAME" uname -m | tr -d '\r')"
case "$ARCH:$guest_arch" in arm64:aarch64|amd64:x86_64) ;; *) echo "guest is $guest_arch but the runtimes are for $ARCH" >&2; exit 1 ;; esac

# 2. Runtimes, placed and checked exactly as warden-runner does.
"$SBX" exec "$NAME" sudo rm -rf -- /tmp/warden-runtime-stage /tmp/warden-runtime /tmp/warden-claude
"$SBX" cp "$RUNTIMES/codex" "$NAME:/tmp/warden-runtime-stage" >/dev/null
"$SBX" exec "$NAME" sudo sh -c 'chmod -R a+rX /tmp/warden-runtime-stage && test -x /tmp/warden-runtime-stage/bin/codex && test -x /tmp/warden-runtime-stage/bin/codex-code-mode-host && test -x /tmp/warden-runtime-stage/codex-path/rg && test -x /tmp/warden-runtime-stage/codex-resources/bwrap && rm -f /tmp/warden-runtime-stage/.warden-sha256 && mv /tmp/warden-runtime-stage /tmp/warden-runtime'
"$SBX" cp "$RUNTIMES/claude/claude" "$NAME:/tmp/warden-claude" >/dev/null
"$SBX" exec "$NAME" sudo chmod 755 /tmp/warden-claude

# 3. The gateway CA (public certificate only) and the manifest the runner reads.
"$SBX" cp "$CA" "$NAME:/tmp/warden-proxy.crt" >/dev/null
"$SBX" exec "$NAME" sudo sh -c 'install -m 644 /tmp/warden-proxy.crt /usr/local/share/ca-certificates/warden-proxy.crt && rm -f /tmp/warden-proxy.crt && update-ca-certificates >/dev/null'
"$SBX" exec "$NAME" sudo sh -c "set -eu; mkdir -p /opt/warden; printf '{\"platform\":\"linux/%s\",\"codex\":{\"version\":\"%s\",\"target\":\"%s\"},\"claude\":{\"version\":\"%s\",\"sha256\":\"%s\"},\"ca\":{\"sha256\":\"%s\"}}\n' '$ARCH' '$CODEX_VERSION' '$CODEX_TARGET' '$CLAUDE_VERSION' \"\$(sha256sum /tmp/warden-claude | cut -d' ' -f1)\" \"\$(sha256sum /usr/local/share/ca-certificates/warden-proxy.crt | cut -d' ' -f1)\" > /opt/warden/guest-manifest.json; chmod 644 /opt/warden/guest-manifest.json; cat /opt/warden/guest-manifest.json"
"$SBX" exec "$NAME" sh -c 'test -x /tmp/warden-runtime/bin/codex && /tmp/warden-runtime/bin/codex --version && test -x /tmp/warden-claude && ls -ld /tmp/warden-runtime /tmp/warden-claude /opt/warden/guest-manifest.json'

# 4. Snapshot as a template (and optionally export a tar for other hosts).
# The snapshot needs a stopped sandbox.
"$SBX" stop "$NAME" >/dev/null
if [ -n "$OUTPUT" ]; then
  "$SBX" template save "$NAME" "$TAG" --output "$OUTPUT"
else
  "$SBX" template save "$NAME" "$TAG"
fi
# 5. The digest the verifier will see is what `sbx inspect` reports for a
# sandbox created from the tag (template inspect is cloud-only in 0.42.1).
PROBE="warden-guest-probe-$(date +%s)"
"$SBX" create --name "$PROBE" --cpus 1 --memory 1024m --template "$TAG" --deny-network '**' --no-share-skills shell >/dev/null
DIGEST="$("$SBX" inspect "$PROBE" --json | sed -n 's/.*"image_digest": *"\([^"]*\)".*/\1/p' | head -1)"
"$SBX" rm --force "$PROBE" >/dev/null 2>&1 || true
case "$DIGEST" in sha256:????????????????????????????????????????????????????????????????) ;; *) echo "could not read the template digest from a probe sandbox (got '$DIGEST')" >&2; exit 1 ;; esac
echo "template $TAG digest $DIGEST"

# 6. Pin it in warden.json so new sandboxes use it; restart Warden afterwards.
CONFIG="$STATE/warden.json"
if [ -f "$CONFIG" ] && command -v python3 >/dev/null 2>&1; then
  python3 - "$CONFIG" "$TAG" "$DIGEST" <<'PY'
import json, os, sys
path, tag, digest = sys.argv[1:4]
with open(path) as f:
    cfg = json.load(f)
cfg.setdefault("sbx", {})["guestImage"] = tag
cfg["sbx"]["guestImageDigest"] = digest
tmp = path + ".tmp"
with open(tmp, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
os.chmod(tmp, 0o600)
os.replace(tmp, path)
print("pinned in", path)
PY
  echo "restart Warden (Ctrl+C the running 'warden start', then start it again) so the runner uses the new template"
else
  echo "Pin it yourself in $CONFIG: sbx.guestImage=\"$TAG\" sbx.guestImageDigest=\"$DIGEST\", then restart Warden."
fi
