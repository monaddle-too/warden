#!/bin/sh
# Install one platform of a published Warden guest image into the sandbox
# runtime and pin it for the Warden services.
#
#   sudo deploy/guest/install.sh ghcr.io/OWNER/warden-guest:sha-<commit> sha256:<digest> [options]
#
# DIGEST may be the platform image manifest digest printed per platform by the
# guest image workflow, or the multi-platform index digest; an index is
# resolved to this host's platform (or --platform) before anything is pulled.
# What gets loaded and pinned is always one platform's image manifest digest,
# which is what `sbx inspect` reports for the loaded copy.
#
# Defaults reproduce the OVH host: the Warden Compose project at
# /opt/warden/current, the private SBX namespace of user `warden` under
# /var/lib/warden/sbx, and a recreate of the policy and runner services. A
# local install (Apple Silicon Mac or an x86_64 Linux host without Compose)
# points the options at its own state root and skips the Compose step:
#
#   deploy/guest/install.sh IMAGE DIGEST --sbx-home ~/.warden/sbx --sbx-user '' \
#       --env ~/.warden/guest.env --no-compose
#
# Options:
#   --platform linux/amd64|linux/arm64  platform to install (default: this host)
#   --compose FILE     Compose file to recreate policy and runner from
#                      (default /opt/warden/current/deploy/chat/compose.yaml)
#   --env FILE         env file that receives WARDEN_GUEST_TEMPLATE and
#                      WARDEN_GUEST_DIGEST (default: .env beside the Compose file)
#   --sbx-home DIR     private SBX HOME/XDG root (default /var/lib/warden/sbx)
#   --sbx-user USER    run sbx as this user through sudo (default warden;
#                      '' runs it as the current user)
#   --sbx PATH         sbx executable (default: sbx on PATH)
#   --no-compose       write the env file but do not touch Compose services
#
# Requires: docker with the containerd image store (logged in to ghcr.io with
# read:packages if the package is private) and the sbx CLI with a responding
# daemon. The image is pulled by digest, loaded into the sandbox runtime's own
# image store (the daemon never needs registry access), and the runner and
# policy services are recreated with the new template and pinned digest.
# Existing sandboxes keep their current image until they are deleted; only
# new sandboxes use the new one.
set -eu
usage() { sed -n '2,42p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
IMAGE="${1:-}"; DIGEST="${2:-}"
[ -n "$IMAGE" ] && [ -n "$DIGEST" ] || usage
shift 2
PLATFORM=""
COMPOSE=/opt/warden/current/deploy/chat/compose.yaml
ENV_FILE=""
SBX_HOME=/var/lib/warden/sbx
SBX_USER=warden
SBX_BIN=sbx
RUN_COMPOSE=1
while [ $# -gt 0 ]; do
  case "$1" in
    --platform) PLATFORM="$2"; shift 2;;
    --compose) COMPOSE="$2"; shift 2;;
    --env) ENV_FILE="$2"; shift 2;;
    --sbx-home) SBX_HOME="$2"; shift 2;;
    --sbx-user) SBX_USER="$2"; shift 2;;
    --sbx) SBX_BIN="$2"; shift 2;;
    --no-compose) RUN_COMPOSE=0; shift;;
    -h|--help) usage;;
    *) echo "unknown option $1" >&2; usage;;
  esac
done
[ -n "$ENV_FILE" ] || ENV_FILE="$(dirname "$COMPOSE")/.env"
case "$DIGEST" in sha256:????????????????????????????????????????????????????????????????) ;; *) echo "digest must be sha256:<64 hex>" >&2; exit 2;; esac
if [ -z "$PLATFORM" ]; then
  case "$(uname -m)" in
    x86_64|amd64) PLATFORM=linux/amd64;;
    aarch64|arm64) PLATFORM=linux/arm64;;
    *) echo "unsupported host architecture $(uname -m); pass --platform" >&2; exit 2;;
  esac
fi
case "$PLATFORM" in linux/amd64|linux/arm64) ;; *) echo "platform must be linux/amd64 or linux/arm64" >&2; exit 2;; esac
NAME="${IMAGE%%@*}"

# sbx runs in the private Warden namespace, as the service user when one is set.
SBX_ENV="HOME=$SBX_HOME/home XDG_CONFIG_HOME=$SBX_HOME/config XDG_DATA_HOME=$SBX_HOME/data XDG_STATE_HOME=$SBX_HOME/state XDG_CACHE_HOME=$SBX_HOME/cache"
sbx_run() {
  if [ -n "$SBX_USER" ]; then sudo -u "$SBX_USER" env $SBX_ENV "$SBX_BIN" "$@"; else env $SBX_ENV "$SBX_BIN" "$@"; fi
}

# An index digest is resolved to the platform's image manifest digest; a
# manifest digest is checked to be an image manifest.
MEDIA="$(docker buildx imagetools inspect "$NAME@$DIGEST" --format '{{.Manifest.MediaType}}')"
case "$MEDIA" in
  application/vnd.oci.image.index.v1+json|application/vnd.docker.distribution.manifest.list.v2+json)
    RESOLVED="$(docker buildx imagetools inspect "$NAME@$DIGEST" \
      --format '{{range .Manifest.Manifests}}{{if .Platform}}{{.Platform.OS}}/{{.Platform.Architecture}} {{.Digest}}{{"\n"}}{{end}}{{end}}' \
      | awk -v p="$PLATFORM" '$1 == p { print $2; exit }')"
    [ -n "$RESOLVED" ] || { echo "$NAME@$DIGEST has no $PLATFORM image" >&2; exit 1; }
    echo "$DIGEST is a multi-platform index; $PLATFORM image manifest is $RESOLVED"
    DIGEST="$RESOLVED";;
  application/vnd.oci.image.manifest.v1+json|application/vnd.docker.distribution.manifest.v2+json)
    ACTUAL_PLATFORM="$(docker buildx imagetools inspect "$NAME@$DIGEST" --format '{{.Image.OS}}/{{.Image.Architecture}}')"
    [ "$ACTUAL_PLATFORM" = "$PLATFORM" ] || { echo "$NAME@$DIGEST is a $ACTUAL_PLATFORM image, not $PLATFORM" >&2; exit 1; };;
  *) echo "unrecognised manifest type $MEDIA for $NAME@$DIGEST" >&2; exit 1;;
esac

echo "pulling $NAME@$DIGEST ($PLATFORM)"
docker pull -q "$NAME@$DIGEST"
docker tag "$NAME@$DIGEST" "$NAME"
ACTUAL="$(docker image inspect "$NAME" --format '{{.Id}}')"
[ "$ACTUAL" = "$DIGEST" ] || { echo "pulled image reports $ACTUAL, expected $DIGEST (is the containerd image store enabled?)" >&2; exit 1; }
TAR="$(mktemp "${TMPDIR:-/var/tmp}/warden-guest.XXXXXX")"
trap 'rm -f "$TAR"' EXIT
# The tag holds exactly the one platform manifest pulled above, so the tar
# carries that manifest and sbx reports its digest after the load.
docker save "$NAME" -o "$TAR"
[ -z "$SBX_USER" ] || chown "$SBX_USER" "$TAR"
sbx_run template load "$TAR"
if sbx_run template ls | grep -qF "$(printf %s "${DIGEST#sha256:}" | cut -c1-12)"; then
  echo "sandbox runtime lists $NAME ($DIGEST)"
else
  echo "warning: sbx template ls does not show ${DIGEST#sha256:} for $NAME; listing follows" >&2
  sbx_run template ls >&2 || true
fi

# Pin the template and digest for the services (portable rewrite; BSD sed
# has no -i without a suffix).
touch "$ENV_FILE"
KEPT="$(grep -v -e '^WARDEN_GUEST_TEMPLATE=' -e '^WARDEN_GUEST_DIGEST=' "$ENV_FILE" || true)"
{ [ -z "$KEPT" ] || printf '%s\n' "$KEPT"; printf 'WARDEN_GUEST_TEMPLATE=%s\nWARDEN_GUEST_DIGEST=%s\n' "$NAME" "$DIGEST"; } > "$ENV_FILE.tmp"
mv "$ENV_FILE.tmp" "$ENV_FILE"
echo "wrote WARDEN_GUEST_TEMPLATE=$NAME and WARDEN_GUEST_DIGEST=$DIGEST to $ENV_FILE"
if [ "$RUN_COMPOSE" = 1 ]; then
  echo "recreating policy and runner with template $NAME (digest $DIGEST)"
  docker compose -f "$COMPOSE" --env-file "$ENV_FILE" up -d policy runner
  docker inspect warden-runner-1 --format '{{join .Args " "}}' | grep -o -- "--template [^ ]*"
  docker inspect warden-policy-1 --format '{{join .Args " "}}' | grep -o -- "--guest-image-digest [^ ]*"
  echo "done; new sandboxes use the guest image"
else
  echo "done; restart the runner with --template $NAME and the policy service with --guest-image-digest $DIGEST (or sbx.guestImage / sbx.guestImageDigest in warden.json)"
fi
