#!/bin/sh
# Build the current checkout as a release and deploy it to the Warden
# instance whose release link is $WARDEN_HOME, then restart a background
# Warden if one is running. A thin wrapper over `warden release build`
# (cmd/warden/release.go, docs/host-dogfood-plan.md): the launcher built
# from this checkout runs scripts/release.sh, unpacks the tarball into the
# shared release store (~/.warden/releases/<name>, once per version),
# repoints <state>/release, runs the new release's own `warden install
# --upgrade` into the instance and restarts.
#
#   WARDEN_HOME=~/.warden/release scripts/deploy-local.sh [--no-restart] [--test]
#
# $WARDEN_HOME is <state>/release; its directory is the instance's state
# (`~/.warden` is the default instance, `~/.warden-NAME` the instance NAME,
# see `warden instance list`). Switching back is `warden release use
# VERSION --state <state>`. --test runs the full test suite first (the
# default skips it: release.sh has already been proven on this commit, or
# you are iterating).
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
STATE="$(dirname "$HOME_DIR")"
[ "$(basename "$HOME_DIR")" = release ] || echo "note: $HOME_DIR is not <state>/release; deploying to the instance at $STATE" >&2

# The launcher that drives the deploy is this checkout's own build.
mkdir -p dist/chat
GOPROXY="${GOPROXY:-off}" GOFLAGS="${GOFLAGS:--mod=mod}" go -C chat build -trimpath -o ../dist/chat/warden ./cmd/warden

set -- --state "$STATE"
[ "$RESTART" = 1 ] && set -- "$@" --restart
[ "$TEST" = 1 ] && set -- "$@" --test
exec dist/chat/warden release build . "$@"
