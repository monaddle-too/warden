#!/bin/sh
# Build a Warden release: the frontend, the warden binary for linux/amd64,
# linux/arm64 and darwin/arm64 with the revision linked in, the macOS menu
# bar item (chat/menu/main.swift, compiled with swiftc when the build host
# has it: the Mac runner does), one tarball per target and SHA256SUMS. .github/workflows/release.yml runs this script on the
# self-hosted Mac runner and adds the server image, the Helm chart and the
# GitHub release; run it by hand for the same tarballs without Actions. With
# --chart it also packages the Helm chart (scripts/package-chart.sh: chart
# version = the tag without its v, appVersion = the tag, values defaulting
# image.tag to the tag) beside the tarballs. With --publish (which implies
# --chart) it creates the GitHub release for the current tag with `gh`
# itself, pushes the chart to oci://ghcr.io/monaddle-too/charts with `helm
# push` and attaches the chart tarball to the release.
#
#   scripts/release.sh [--version vX.Y.Z] [--skip-tests] [--chart] [--publish]
#
# The server image (deploy/chat/Dockerfile) needs a Docker host and is not
# built here; OVH builds it on the server (deploy/chat/README.md). A chart
# published from here therefore pulls ghcr.io/monaddle-too/warden:<tag> by
# tag, which someone must build and push before the chart is installed.
# The chart push needs your own `helm registry login ghcr.io`; the script
# checks for one and never logs in with a stored token.
set -eu
cd "$(dirname "$0")/.."

CHARTS="oci://ghcr.io/monaddle-too/charts" # helm push appends /warden
VERSION=""
SKIP_TESTS=0
CHART=0
PUBLISH=0
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --skip-tests) SKIP_TESTS=1; shift ;;
    --chart) CHART=1; shift ;;
    --publish) PUBLISH=1; CHART=1; shift ;;
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
if [ "$CHART" = 1 ] && ! command -v helm >/dev/null 2>&1; then
  echo "--chart needs helm on PATH" >&2; exit 2
fi
if [ "$PUBLISH" = 1 ]; then
  # helm push reads helm's own registry config and falls back to Docker's;
  # either must already name ghcr.io. Logging in is yours: it takes a
  # GitHub token with write:packages, which this script never handles.
  helm_config="$(helm env HELM_REGISTRY_CONFIG 2>/dev/null || true)"
  [ -n "$helm_config" ] || helm_config="$HOME/.config/helm/registry/config.json"
  docker_config="${DOCKER_CONFIG:-$HOME/.docker}/config.json"
  if ! { [ -f "$helm_config" ] && grep -q '"ghcr.io"' "$helm_config"; } \
     && ! { [ -f "$docker_config" ] && grep -q '"ghcr.io"' "$docker_config"; }; then
    cat >&2 <<EOF
--publish pushes the chart to $CHARTS and needs a registry login for
ghcr.io, and neither $helm_config nor
$docker_config names it. Log in yourself first, with a GitHub token that
has the write:packages scope (it prompts for the token):

  helm registry login ghcr.io --username <your GitHub user>

then run this script again. It never logs in for you.
EOF
    exit 2
  fi
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

# Go resolves from the module cache unless the caller sets GOPROXY (CI does).
export GOPROXY="${GOPROXY:-off}" GOFLAGS=-mod=mod CGO_ENABLED=0
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

# The menu bar item, darwin/arm64 only: a Swift executable beside warden
# in bin/ (warden install registers it as a launchd agent; a tarball
# without it installs without the item, and warden install says so).
# Xcode's swiftc is the one that links AppKit; a build host without it
# ships no item.
if command -v xcrun >/dev/null 2>&1 && xcrun --find swiftc >/dev/null 2>&1; then
  xcrun swiftc -O -target arm64-apple-macos14.0 -framework AppKit \
    -o dist/darwin-arm64/warden-menu chat/menu/main.swift
  echo "built dist/darwin-arm64/warden-menu"
else
  echo "no swiftc: the darwin tarball ships without the menu bar item" >&2
fi

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
  [ ! -f "dist/$target/warden-menu" ] || cp "dist/$target/warden-menu" "$stage/bin/"
  cp -R chat/web/dist "$stage/web"
  cp config/policy.template.json "$stage/config/"
  cp vendor/github-operations.json vendor/github-meta.json "$stage/vendor/"
  cp deploy/chat/warden.example.json "$stage/config/warden.server.example.json"
  chmod -R a+rX "$stage"
  if ! tar --version 2>/dev/null | grep -q GNU; then
    find "$stage" -exec touch -m -d "@$mtime" {} + 2>/dev/null || find "$stage" -exec touch -m -t "$(date -r "$mtime" +%Y%m%d%H%M.%S)" {} +
  fi
  # shellcheck disable=SC2086
  COPYFILE_DISABLE=1 tar -C dist/stage $TARFLAGS --no-xattrs -czf "dist/release/$name.tar.gz" "$name"
done
(cd dist/release && { command -v sha256sum >/dev/null 2>&1 && sha256sum -- *.tar.gz || shasum -a 256 -- *.tar.gz; } > SHA256SUMS && cat SHA256SUMS)

# The chart, versioned with the binary: warden-<tag without v>.tgz beside
# the tarballs and in SHA256SUMS. No image is built here, so its values
# pin no digest; they default to ghcr.io/monaddle-too/warden:<tag>.
CHART_TGZ=""
CHART_VERSION="${VERSION#v}"
if [ "$CHART" = 1 ]; then
  CHART_TGZ="$(scripts/package-chart.sh --version "$VERSION" --out dist/release)"
  (cd dist/release && { command -v sha256sum >/dev/null 2>&1 && sha256sum -- "$(basename "$CHART_TGZ")" || shasum -a 256 -- "$(basename "$CHART_TGZ")"; } >> SHA256SUMS && tail -1 SHA256SUMS)
fi

# An unpacked tarball must work from bin/ without configuration.
if [ -f "dist/release/warden-${VERSION}-${host}.tar.gz" ]; then
  rm -rf dist/check; mkdir -p dist/check
  tar -C dist/check -xzf "dist/release/warden-${VERSION}-${host}.tar.gz"
  "dist/check/warden-${VERSION}-${host}/bin/warden" policy selfcheck
  "dist/check/warden-${VERSION}-${host}/bin/warden" --version
fi

if [ "$PUBLISH" = 1 ]; then
  # The chart first, so the notes carry its OCI digest; a re-run after a
  # failed release step pushes the same tag again, which GHCR allows.
  helm push "$CHART_TGZ" "$CHARTS" > dist/release/chart-push.txt 2>&1 || { cat dist/release/chart-push.txt >&2; exit 1; }
  cat dist/release/chart-push.txt
  CHART_DIGEST="$(sed -n 's/^Digest: //p' dist/release/chart-push.txt)"
  {
    echo "## Warden $VERSION"
    echo
    echo "Built locally with scripts/release.sh from $(git rev-parse HEAD). The server image is built on the server (deploy/chat/README.md)."
    echo
    echo "### Helm chart"
    echo
    echo "\`$CHARTS/warden\` version \`$CHART_VERSION\` (OCI digest \`$CHART_DIGEST\`; also \`$(basename "$CHART_TGZ")\` below). Its values default to \`ghcr.io/monaddle-too/warden:$VERSION\` by tag, which this build does not push: build and push that image before installing (docs/warden-kubernetes.md)."
    echo
    echo '```'
    echo "helm install warden $CHARTS/warden --version $CHART_VERSION -n warden --create-namespace -f values.yaml"
    echo '```'
    echo
    echo "### Tarballs and chart"
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
  gh release upload "$VERSION" "$CHART_TGZ" --clobber
fi
echo "release artefacts in dist/release"
if [ "$CHART" = 1 ]; then
  echo "chart $CHART_TGZ (version $CHART_VERSION, appVersion $VERSION); a Kubernetes install also needs the image ghcr.io/monaddle-too/warden:$VERSION on GHCR"
fi
