#!/bin/sh
# Install Warden on this machine: download the newest release for this
# host from GitHub, verify it against the release's SHA256SUMS, unpack it
# under ~/.warden/releases, point ~/.warden/release at it and run
# `warden install` (which sets up the private SBX namespace and asks the
# one or two questions it has). Safe to re-run: a later run switches the
# link to the newer release and `warden install` reports "already" for
# what exists.
#
#   sh -c "$(curl -fsSL https://raw.githubusercontent.com/monaddle-too/warden/main/install.sh)"
#
# Run it that way (not `curl | sh`) so that `warden install` keeps the
# terminal for the Docker device sign-in.
#
# Environment: WARDEN_VERSION pins a release tag (default: the newest);
# WARDEN_HOME is the launcher directory (default ~/.warden/release; its
# parent holds the state and the unpacked releases). --no-install only
# downloads and links.
set -eu

REPO="monaddle-too/warden"
HOME_DIR="${WARDEN_HOME:-$HOME/.warden/release}"
INSTALL=1
for arg in "$@"; do
  case "$arg" in
    --no-install) INSTALL=0 ;;
    *) echo "unknown argument $arg" >&2; exit 2 ;;
  esac
done
case "$HOME_DIR" in /*) ;; *) echo "WARDEN_HOME must be an absolute path: $HOME_DIR" >&2; exit 2 ;; esac
RELEASES="$(dirname "$HOME_DIR")/releases"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
case "$os-$arch" in
  darwin-arm64|linux-amd64|linux-arm64) ;;
  *) echo "no Warden release for $os/$arch: Warden runs on Apple Silicon Macs and x86_64 Linux with KVM" >&2; exit 1 ;;
esac
command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }
if command -v sha256sum >/dev/null; then SUM="sha256sum"; else SUM="shasum -a 256"; fi

VERSION="${WARDEN_VERSION:-}"
if [ -z "$VERSION" ]; then
  # Releases are pre-releases while Warden is in alpha, which GitHub's
  # "latest" download URL skips; the newest entry of the list is the one.
  VERSION="$(curl -fsSL "https://api.github.com/repos/$REPO/releases?per_page=1" \
    | grep -m1 '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')"
  [ -n "$VERSION" ] || { echo "could not find the newest release of $REPO" >&2; exit 1; }
fi
NAME="warden-$VERSION-$os-$arch"
TARBALL="$NAME.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"

if [ -d "$RELEASES/$NAME" ] && [ -x "$RELEASES/$NAME/bin/warden" ]; then
  echo "$VERSION is already unpacked at $RELEASES/$NAME"
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  echo "downloading $TARBALL"
  curl -fsSL -o "$tmp/$TARBALL" "$BASE/$TARBALL" && curl -fsSL -o "$tmp/SHA256SUMS" "$BASE/SHA256SUMS" \
    || { echo "no release $VERSION for $os/$arch at https://github.com/$REPO/releases" >&2; exit 1; }
  (cd "$tmp" && grep " $TARBALL\$" SHA256SUMS | $SUM -c - >/dev/null) \
    || { echo "$TARBALL does not match the release's SHA256SUMS" >&2; exit 1; }
  mkdir -p "$RELEASES"
  rm -rf "$RELEASES/$NAME"
  tar -C "$RELEASES" -xzf "$tmp/$TARBALL"
  [ -x "$RELEASES/$NAME/bin/warden" ] || { echo "$TARBALL did not contain $NAME/bin/warden" >&2; exit 1; }
fi
ln -sfn "$RELEASES/$NAME" "$HOME_DIR"
echo "installed $VERSION at $HOME_DIR"
"$HOME_DIR/bin/warden" version

if [ "$INSTALL" = 1 ]; then
  echo
  "$HOME_DIR/bin/warden" install
fi

case ":$PATH:" in
  *":$HOME_DIR/bin:"*) ;;
  *)
    echo
    echo "Add the launcher to your PATH, for example in your shell profile:"
    echo "  export PATH=\"$HOME_DIR/bin:\$PATH\""
    ;;
esac
