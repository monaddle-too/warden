#!/usr/bin/env bash
# Root-owned entry point for the dedicated CI SSH key. Deployment bundles contain
# trusted application/Compose code and therefore have deployment-level privilege.
set -Eeuo pipefail
umask 077
root=/opt/ocsf
command=${1:-}
if [[ ! "$command" =~ ^(deploy|rollback)\ ([0-9a-f]{40})$ ]]; then
  echo 'Expected: deploy <40-character commit SHA> or rollback <SHA>' >&2
  exit 64
fi
mode=${BASH_REMATCH[1]}
revision=${BASH_REMATCH[2]}
mkdir -p "$root/releases"
exec 9>"$root/deploy.lock"
flock -w 600 9
release="$root/releases/$revision"
if [[ "$mode" == deploy ]]; then
  incoming=$(mktemp -d "$root/.incoming.XXXXXX")
  trap 'rm -rf -- "$incoming"' EXIT
  head -c 268435457 > "$incoming/bundle.tar"
  [[ $(wc -c < "$incoming/bundle.tar") -le 268435456 ]] || { echo 'Bundle exceeds 256 MiB' >&2; exit 65; }
  python3 - "$incoming" <<'PY'
import pathlib, sys, tarfile
root = pathlib.Path(sys.argv[1])
expected = {'image.tar.gz', 'compose.yaml', 'Caddyfile'}
with tarfile.open(root / 'bundle.tar', 'r:') as archive:
    members = archive.getmembers()
    if len(members) != 3 or {m.name for m in members} != expected:
        raise SystemExit('Unexpected release archive contents')
    if any(not m.isfile() or m.size > 256 * 1024 * 1024 for m in members):
        raise SystemExit('Unsafe release archive entry')
    for member in members:
        with archive.extractfile(member) as src, (root / member.name).open('wb') as dst:
            import shutil
            shutil.copyfileobj(src, dst)
PY
  rm "$incoming/bundle.tar"
  if [[ -d "$release" ]]; then
    for file in compose.yaml Caddyfile image.tar.gz; do
      cmp "$incoming/$file" "$release/$file" || { echo 'A different bundle already exists for this revision' >&2; exit 65; }
    done
  else
    mkdir "$release"
    cp "$incoming/"{compose.yaml,Caddyfile,image.tar.gz} "$release/"
  fi
fi
test -f "$release/image.tar.gz"
gzip -dc "$release/image.tar.gz" | docker load
image="ocsf-explorer:$revision"
[[ $(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' "$image") == "$revision" ]]
printf 'APP_IMAGE=%s\nAPP_REVISION=%s\n' "$image" "$revision" > "$release/release.env"
compose() {
  docker compose --project-name ocsf --env-file "$root/config.env" --env-file "$1/release.env" -f "$1/compose.yaml" "${@:2}"
}
compose "$release" config --quiet
previous=''
if [[ -L "$root/current" ]]; then previous=$(readlink -f "$root/current"); fi
restore() {
  code=$?
  trap - ERR
  echo "Deployment failed (exit $code); restoring previous release" >&2
  compose "$release" logs --tail 30 >&2 || true
  if [[ -n "$previous" && -d "$previous" ]]; then
    if compose "$previous" up -d --remove-orphans --wait --wait-timeout 120; then
      echo "Restored $(basename "$previous")" >&2
    else
      echo 'ROLLBACK FAILED: operator intervention required' >&2
    fi
  else
    compose "$release" down || true
  fi
  exit "$code"
}
trap restore ERR
compose "$release" up -d --remove-orphans --wait --wait-timeout 120
actual=$(curl --fail --silent --show-error --max-time 10 http://127.0.0.1:8080/healthz)
python3 -c 'import json,sys; assert json.loads(sys.argv[1])["revision"] == sys.argv[2]' "$actual" "$revision"
site_host=$(sed -n 's/^SITE_HOST=//p' "$root/config.env")
# Initial ACME issuance can take a little longer than an app restart.
curl --fail --silent --show-error --max-time 15 --retry 12 --retry-delay 5 --retry-all-errors "https://$site_host/healthz" > "$release/health.json"
python3 -c 'import json,sys; assert json.load(open(sys.argv[1]))["revision"] == sys.argv[2]' "$release/health.json" "$revision"
curl --fail --silent --show-error --max-time 15 "https://$site_host/" > "$release/smoke.html"
grep -q OCSF "$release/smoke.html"
if [[ -n "$previous" && "$previous" != "$release" ]]; then
  ln -sfn "$previous" "$root/previous"
fi
ln -sfn "$release" "$root/current.next"
mv -Tf "$root/current.next" "$root/current"
trap - ERR
echo "Deployed $revision to https://$site_host"
