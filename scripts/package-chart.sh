#!/bin/sh
# Package the Helm chart (deploy/helm/warden) for one release, the same way
# from .github/workflows/release.yml and from scripts/release.sh --chart
# (docs/warden-kubernetes-plan.md, decision 17): the chart version is the
# release tag without its leading v (v0.1.0-alpha.9 -> 0.1.0-alpha.9),
# appVersion is the tag, and the packaged values default image.tag to the
# tag and image.digest to the server image's index digest when the caller
# has it (the workflow does; a local build has no image). The checkout's
# Chart.yaml and values.yaml keep their 0.0.0-dev placeholders.
#
#   scripts/package-chart.sh --version vX.Y.Z [--image-digest sha256:<64 hex>]
#       [--image-repository ghcr.io/<owner>/warden] [--out dist/release]
#
# Writes <out>/warden-<chart version>.tgz, lints it and checks that it
# renders the release's image reference, then prints its path. Pushing it
# (helm push <tgz> oci://ghcr.io/<owner>/charts) is the caller's, with a
# registry login this script never performs.
set -eu
cd "$(dirname "$0")/.."

VERSION=""
DIGEST=""
REPOSITORY=""
OUT="dist/release"
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --image-digest) DIGEST="$2"; shift 2 ;;
    --image-repository) REPOSITORY="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
case "$VERSION" in v?*) ;; *) echo "--version must be a vX.Y.Z tag, got '$VERSION'" >&2; exit 2 ;; esac
CHART_VERSION="${VERSION#v}"
if [ -n "$DIGEST" ] && ! printf '%s\n' "$DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$'; then
  echo "--image-digest must be sha256:<64 hex>, got '$DIGEST'" >&2; exit 2
fi
command -v helm >/dev/null 2>&1 || { echo "helm not found on PATH" >&2; exit 1; }

src="deploy/helm/warden"
stage="$OUT/chart/warden"
rm -rf "$OUT/chart"; mkdir -p "$stage"
cp -R "$src"/. "$stage"/

# Only the image block's defaults change; every other line of values.yaml
# is the checkout's, so the published chart documents itself as the tree
# does. The edit is checked below: a reworded values.yaml must not ship a
# chart that silently kept the placeholders.
awk -v tag="$VERSION" -v digest="$DIGEST" -v repo="$REPOSITORY" '
  /^image:/ { in_image = 1; print; next }
  in_image && /^[^ ]/ { in_image = 0 }
  in_image && repo != "" && /^  repository: / { print "  repository: " repo; next }
  in_image && /^  tag: ""/ { print "  tag: \"" tag "\""; next }
  in_image && digest != "" && /^  digest: ""/ { print "  digest: \"" digest "\""; next }
  { print }
' "$src/values.yaml" > "$stage/values.yaml"
grep -q "^  tag: \"$VERSION\"\$" "$stage/values.yaml" || { echo "values.yaml: image.tag was not set to $VERSION" >&2; exit 1; }
if [ -n "$DIGEST" ]; then
  grep -q "^  digest: \"$DIGEST\"\$" "$stage/values.yaml" || { echo "values.yaml: image.digest was not set to $DIGEST" >&2; exit 1; }
fi
if [ -n "$REPOSITORY" ]; then
  grep -q "^  repository: $REPOSITORY\$" "$stage/values.yaml" || { echo "values.yaml: image.repository was not set to $REPOSITORY" >&2; exit 1; }
fi

helm package "$stage" --version "$CHART_VERSION" --app-version "$VERSION" -d "$OUT" >/dev/null
tgz="$OUT/warden-$CHART_VERSION.tgz"
test -f "$tgz" || { echo "helm package did not write $tgz" >&2; exit 1; }
rm -rf "$OUT/chart"

# The package must lint and render the release's image: by digest when
# one was given, else by tag.
helm lint "$tgz" -f "$src/testdata/values-gvisor-loopback.yaml" --quiet
repo="$REPOSITORY"
[ -n "$repo" ] || repo="$(awk '/^image:/{f=1;next} f&&/^  repository: /{print $2;exit} f&&/^[^ ]/{exit}' "$src/values.yaml")"
if [ -n "$DIGEST" ]; then want="$repo@$DIGEST"; else want="$repo:$VERSION"; fi
helm template warden "$tgz" --namespace warden -f "$src/testdata/values-gvisor-loopback.yaml" -s templates/deployments.yaml \
  | grep -q "image: \"$want\"" || { echo "the packaged chart does not render image $want" >&2; exit 1; }
helm show chart "$tgz" | grep -q "^appVersion: $VERSION\$" || { echo "the packaged chart's appVersion is not $VERSION" >&2; exit 1; }
echo "$tgz"
