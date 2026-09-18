#!/bin/sh
# Build the current checkout as a release and deploy it to $WARDEN_HOME, the
# directory the operator's `warden` runs from, then restart a background
# Warden if one is running.
#
#   WARDEN_HOME=~/.warden/release scripts/deploy-local.sh [--no-restart] [--test]
#
# $WARDEN_HOME is a symlink to one unpacked release under
# <state>/releases/<version>; each deploy unpacks beside the previous ones
# and repoints the link, so switching back is `ln -sfn` to an older
# directory. --test runs the full test suite first (the default skips it,
# release.sh has already been proven on this commit or you are iterating).
set -eu
cd "$(dirname "$0")/.."

HOME_DIR="${WARDEN_HOME:-$HOME/.warden/release}"
RESTART=1
TEST=0
while [ $# -gt 0 ]; do
  case "$1" in
    --no-restart) RESTART=0; shift ;;
    --test) TEST=1; shift ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
case "$HOME_DIR" in /*) ;; *) echo "WARDEN_HOME must be an absolute path: $HOME_DIR" >&2; exit 2 ;; esac
RELEASES="$(dirname "$HOME_DIR")/releases"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
if [ "$TEST" = 1 ]; then
  ./scripts/release.sh
else
  ./scripts/release.sh --skip-tests
fi
VERSION="$(git describe --tags --exact-match 2>/dev/null || echo "v0.0.0-dev.$(git rev-parse --short=12 HEAD)")"
TARBALL="dist/release/warden-$VERSION-$os-$arch.tar.gz"
[ -f "$TARBALL" ] || { echo "no tarball for this host: $TARBALL" >&2; exit 1; }

mkdir -p "$RELEASES"
rm -rf "$RELEASES/warden-$VERSION-$os-$arch"
tar -C "$RELEASES" -xzf "$TARBALL"
ln -sfn "$RELEASES/warden-$VERSION-$os-$arch" "$HOME_DIR"
echo "deployed $VERSION to $HOME_DIR"
"$HOME_DIR/bin/warden" version

if [ "$RESTART" = 1 ]; then
  status="$("$HOME_DIR/bin/warden" status 2>/dev/null || true)"
  if echo "$status" | grep -qE '^service: .*: running'; then
    "$HOME_DIR/bin/warden" restart
  elif echo "$status" | grep -q 'running detached'; then
    "$HOME_DIR/bin/warden" stop
    "$HOME_DIR/bin/warden" start --detach
  else
    echo "no Warden running; start one with: warden start (the service) or warden start --detach"
  fi
fi
