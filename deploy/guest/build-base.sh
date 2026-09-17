#!/usr/bin/env bash
# Build the Warden guest base image (Dockerfile.base) locally, with docker or
# nerdctl, and print the reference and digest to pin.
#
#   deploy/guest/build-base.sh [--builder docker|nerdctl] [--platform linux/amd64|linux/arm64]
#                              [--tag NAME:TAG] [--k3s] [--address SOCKET] [--no-cache]
#
#   --builder   docker or nerdctl (default: docker if on PATH, else nerdctl;
#               --k3s always uses nerdctl)
#   --platform  the target platform (default: the builder's own platform)
#   --tag       the image reference (default warden-guest-base:<git describe>)
#   --k3s       build into the k8s.io containerd namespace of a k3s node
#               (`nerdctl --namespace k8s.io build`), so pods on that node
#               run the image without a registry; needs nerdctl and a
#               running buildkitd on the node
#   --address   the containerd socket for nerdctl (default with --k3s:
#               /run/k3s/containerd/containerd.sock; otherwise nerdctl's own)
#   --no-cache  rebuild every layer
#
# The only network use is the build itself (the Dockerfile fetches the
# pinned runtimes from their releases and apt packages from Ubuntu). When
# the containerd socket is root-only the builder runs through sudo.
#
# Output: the image reference, its image ID and, when the store records
# one, its manifest digest, which is the value to pin as
# kubernetes.guestImageDigest (the kubelet reports it as the container's
# imageID). Docker's classic image store has no manifest digest for a local
# build; push the image or use the containerd image store to get one.
set -euo pipefail
SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
cd "$(dirname "$SELF")"

BUILDER=""
PLATFORM=""
TAG=""
K3S=0
ADDRESS=""
NO_CACHE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --builder) BUILDER="$2"; shift 2;;
    --platform) PLATFORM="$2"; shift 2;;
    --tag) TAG="$2"; shift 2;;
    --k3s) K3S=1; shift;;
    --address) ADDRESS="$2"; shift 2;;
    --no-cache) NO_CACHE=1; shift;;
    -h|--help) sed -n '2,28p' "$SELF" | sed 's/^# \{0,1\}//'; exit 0;;
    *) echo "unknown option $1" >&2; exit 2;;
  esac
done

[ -f Dockerfile.base ] || { echo "Dockerfile.base is missing beside this script" >&2; exit 1; }
if [ -n "$PLATFORM" ]; then
  case "$PLATFORM" in linux/amd64|linux/arm64) ;; *) echo "--platform must be linux/amd64 or linux/arm64" >&2; exit 2;; esac
fi
if [ "$K3S" = 1 ]; then
  if [ -n "$BUILDER" ] && [ "$BUILDER" != nerdctl ]; then echo "--k3s builds with nerdctl, not $BUILDER" >&2; exit 2; fi
  BUILDER=nerdctl
  [ -n "$ADDRESS" ] || ADDRESS=/run/k3s/containerd/containerd.sock
fi
if [ -z "$BUILDER" ]; then
  if command -v docker >/dev/null 2>&1; then BUILDER=docker
  elif command -v nerdctl >/dev/null 2>&1; then BUILDER=nerdctl
  else echo "neither docker nor nerdctl is on PATH" >&2; exit 1; fi
fi
case "$BUILDER" in docker|nerdctl) ;; *) echo "--builder must be docker or nerdctl" >&2; exit 2;; esac
command -v "$BUILDER" >/dev/null 2>&1 || { echo "$BUILDER is not on PATH" >&2; exit 1; }
if [ -z "$TAG" ]; then
  DESCRIBE="$(git describe --tags --always --dirty 2>/dev/null || true)"
  [ -n "$DESCRIBE" ] || { echo "not in a git checkout; pass --tag" >&2; exit 1; }
  TAG="warden-guest-base:${DESCRIBE}"
fi
case "$TAG" in *:*) ;; *) TAG="$TAG:latest";; esac

# The builder command: nerdctl with its namespace and socket, through sudo
# when the socket is root-only.
BUILD=("$BUILDER")
if [ "$BUILDER" = nerdctl ]; then
  [ -z "$ADDRESS" ] || BUILD+=(--address "$ADDRESS")
  [ "$K3S" = 0 ] || BUILD+=(--namespace k8s.io)
  if [ -n "$ADDRESS" ] && [ -S "$ADDRESS" ] && [ ! -w "$ADDRESS" ]; then
    BUILD=(sudo "${BUILD[@]}")
  elif [ -n "$ADDRESS" ] && [ ! -e "$ADDRESS" ]; then
    echo "containerd socket $ADDRESS does not exist (is k3s running on this node?)" >&2; exit 1
  fi
fi

ARGS=(build -f Dockerfile.base -t "$TAG")
[ -z "$PLATFORM" ] || ARGS+=(--platform "$PLATFORM")
[ "$NO_CACHE" = 0 ] || ARGS+=(--no-cache)
if [ "$BUILDER" = docker ]; then
  # Load the result into the local store whichever builder driver is
  # current (a docker-container buildx driver otherwise keeps it in cache).
  ARGS+=(--load)
fi
echo "building $TAG with ${BUILD[*]}${PLATFORM:+ for $PLATFORM}"
"${BUILD[@]}" "${ARGS[@]}" .

# What was built: the image ID and the manifest digest the store records.
ID="$("${BUILD[@]}" image inspect --format '{{.Id}}' "$TAG")"
DIGESTS="$("${BUILD[@]}" image inspect --format '{{range .RepoDigests}}{{.}} {{end}}' "$TAG")"
MANIFEST=""
for d in $DIGESTS; do
  case "$d" in *@sha256:*) MANIFEST="${d##*@}"; break;; esac
done
echo "image: $TAG"
echo "id: $ID"
if [ -n "$MANIFEST" ]; then
  echo "digest: $MANIFEST (manifest digest; pin as kubernetes.guestImageDigest)"
else
  echo "digest: none recorded for a local build in this store (push the image, or use the containerd image store, to get the manifest digest to pin)"
fi
if [ "$K3S" = 1 ]; then
  echo "the image is in the k8s.io namespace of $ADDRESS; a pod on this node runs it with image: $TAG and imagePullPolicy: Never (or IfNotPresent)"
fi
