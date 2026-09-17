#!/bin/sh
# Build a Warden release locally, without GitHub Actions: the frontend, the
# warden binary for linux/amd64, linux/arm64 and darwin/arm64 with the
# revision linked in, one tarball per target and SHA256SUMS, exactly as
# .github/workflows/release.yml does. With --publish it creates the GitHub
# release for the current tag with `gh`. GitHub Releases cost no Actions
# minutes, so this is the complete path when the Actions budget is exhausted.
#
#   scripts/release.sh [--version vX.Y.Z] [--skip-tests] [--publish]
#
# The server image (deploy/chat/Dockerfile) needs a Docker host and is not
# built here; OVH builds it on the server (deploy/chat/README.md).
set -eu
cd "$(dirname "$0")/.."

VERSION=""
SKIP_TESTS=0
PUBLISH=0
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --skip-tests) SKIP_TESTS=1; shift ;;
    --publish) PUBLISH=1; shift ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
if [ -z "$VERSION" ]; then
  if tag="$(git describe --tags --exact-match 2>/dev/null)"; then
    VERSION="$tag"
  else
    VERSION="v0.0.0-dev.$(git rev-parse --short=12 HEAD)"
  fi
fi
case "$VERSION" in v*) ;; *) echo "version must start with v: $VERSION" >&2; exit 2 ;; esac
if [ "$PUBLISH" = 1 ] && ! git describe --tags --exact-match >/dev/null 2>&1; then
  echo "--publish needs HEAD to be at the tag $VERSION" >&2; exit 2
fi
echo "Warden $VERSION ($(git rev-parse HEAD))"

# pnpm: corepack when it works, else the copy corepack cached earlier.
if command -v pnpm >/dev/null 2>&1; then
  PNPM="pnpm"
elif corepack pnpm --version >/dev/null 2>&1; then
  PNPM="corepack pnpm"
else
  cached="$(ls -d "$HOME"/.cache/node/corepack/v1/pnpm/*/bin/pnpm.cjs 2>/dev/null | sort -V | tail -1 || true)"
  [ -n "$cached" ] || { echo "pnpm not found (install it or run corepack enable)" >&2; exit 1; }
  PNPM="node $cached"
fi
$PNPM --dir chat/web install --frozen-lockfile
$PNPM --dir chat/web build
test -f chat/web/dist/index.html

# Go runs from the module cache; nothing is fetched.
export GOPROXY=off GOFLAGS=-mod=mod CGO_ENABLED=0
if [ "$SKIP_TESTS" != 1 ]; then
  test -z "$(gofmt -l chat)"
  go -C chat vet ./...
  go -C chat test -race ./...
fi

CMDS="warden"
for target in linux/amd64 linux/arm64 darwin/arm64; do
  os="${target%/*}"; arch="${target#*/}"
  out="dist/${os}-${arch}"
  rm -rf "$out"; mkdir -p "$out"
  for cmd in $CMDS; do
    GOOS="$os" GOARCH="$arch" go -C chat build -trimpath \
      -ldflags "-s -w -X warden/chat/internal/release.Revision=${VERSION}" \
      -o "../$out/$cmd" "./cmd/$cmd"
  done
done

# The binary for this host runs here: it and each service subcommand must
# report the version.
host="$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
if [ -d "dist/$host" ]; then
  "dist/$host/warden" --version | grep -q "^warden ${VERSION} protocol=" || { echo "warden does not report ${VERSION}" >&2; exit 1; }
  for svc in policy runner serve edge; do
    "dist/$host/warden" $svc --version | grep -q " ${VERSION} protocol=" || { echo "warden $svc does not report ${VERSION}" >&2; exit 1; }
  done
fi

# Tarballs: reproducible ownership and mtimes with GNU or BSD tar.
mtime="$(git log -1 --format=%ct)"
if tar --version 2>/dev/null | grep -q GNU; then
  TARFLAGS="--owner=0 --group=0 --numeric-owner --mtime=@$mtime"
else
  TARFLAGS="--uid 0 --gid 0"
fi
rm -rf dist/release dist/stage; mkdir -p dist/release dist/stage
for target in linux-amd64 linux-arm64 darwin-arm64; do
  name="warden-${VERSION}-${target}"
  stage="dist/stage/$name"
  mkdir -p "$stage/bin" "$stage/config" "$stage/vendor"
  for cmd in $CMDS; do cp "dist/$target/$cmd" "$stage/bin/"; done
  cp -R chat/web/dist "$stage/web"
  cp config/policy.template.json "$stage/config/"
  cp vendor/github-operations.json vendor/github-meta.json "$stage/vendor/"
  cp deploy/chat/warden.example.json "$stage/config/warden.server.example.json"
  chmod -R a+rX "$stage"
  if ! tar --version 2>/dev/null | grep -q GNU; then
    find "$stage" -exec touch -m -d "@$mtime" {} + 2>/dev/null || find "$stage" -exec touch -m -t "$(date -r "$mtime" +%Y%m%d%H%M.%S)" {} +
  fi
  # shellcheck disable=SC2086
  tar -C dist/stage $TARFLAGS -czf "dist/release/$name.tar.gz" "$name"
done
(cd dist/release && { command -v sha256sum >/dev/null 2>&1 && sha256sum -- *.tar.gz || shasum -a 256 -- *.tar.gz; } > SHA256SUMS && cat SHA256SUMS)

# An unpacked tarball must work from bin/ without configuration.
if [ -f "dist/release/warden-${VERSION}-${host}.tar.gz" ]; then
  rm -rf dist/check; mkdir -p dist/check
  tar -C dist/check -xzf "dist/release/warden-${VERSION}-${host}.tar.gz"
  "dist/check/warden-${VERSION}-${host}/bin/warden" policy selfcheck
  "dist/check/warden-${VERSION}-${host}/bin/warden" --version
fi

if [ "$PUBLISH" = 1 ]; then
  {
    echo "## Warden $VERSION"
    echo
    echo "Built locally with scripts/release.sh from $(git rev-parse HEAD). The server image is built on the server (deploy/chat/README.md)."
    echo
    echo '```'
    cat dist/release/SHA256SUMS
    echo '```'
    echo
    echo "Unpack one and run \`bin/warden install\` then \`bin/warden start\` (docs/warden-local-install.md)."
  } > dist/release/notes.md
  PRERELEASE=""
  case "$VERSION" in *-*) PRERELEASE="--prerelease" ;; esac
  # shellcheck disable=SC2086
  gh release create "$VERSION" --title "Warden $VERSION" --notes-file dist/release/notes.md --verify-tag $PRERELEASE \
    dist/release/*.tar.gz dist/release/SHA256SUMS
fi
echo "release artefacts in dist/release"
