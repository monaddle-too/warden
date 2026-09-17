#!/bin/sh
# Entrypoint of the Warden guest base image (/opt/warden/bin/guest-init, run
# under tini). It makes the agent home usable when a volume is mounted over
# it and then executes the container command (by default `sleep infinity`,
# which keeps the pod alive for the runner's exec sessions).
#
# In a Kubernetes sandbox /home/agent is the workspace PersistentVolumeClaim
# and the only persisted path. A freshly provisioned volume arrives empty
# and, depending on the StorageClass, owned by root; the image's copy of
# the home (dotfiles from /etc/skel) is hidden under the mount. Nothing here
# is fatal: a home that cannot be prepared is reported and the command
# still runs, so the runner sees the real error from its first exec.
set -u
home=/home/agent
uid="$(id -u)"
as_root() { if [ "$uid" = 0 ]; then "$@"; else sudo -n "$@"; fi; }
if [ -d "$home" ]; then
  if [ "$(stat -c %u "$home" 2>/dev/null)" != 1000 ] && [ ! -w "$home" ]; then
    as_root chown 1000:1000 "$home" || echo "guest-init: could not take ownership of $home" >&2
  fi
  if [ -z "$(ls -A "$home" 2>/dev/null)" ]; then
    as_root sh -c 'cp -a /etc/skel/. "$1"/ && chown -R 1000:1000 "$1"' seed "$home" \
      || echo "guest-init: could not seed $home from /etc/skel" >&2
  fi
else
  echo "guest-init: $home is missing" >&2
fi
exec "$@"
