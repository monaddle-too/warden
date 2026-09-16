#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ "${WARDEN_SKIP_NATIVE_BUILD:-0}" != 1 ]]; then bash scripts/build-native.sh; fi
task_state=${WARDEN_STATE:-$PWD/.local}
mkdir -p dist
stage=$(mktemp -d "$PWD/.local/package.XXXXXX")
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/warden"
for item in README.md warden host proxy native scripts config schemas docs tests vendor; do
  rsync -a --exclude .build --exclude __pycache__ --exclude '*.pyc' "$item" "$stage/warden/"
done
mkdir -p "$stage/warden/.local/bin"
mkdir -p "$stage/warden/.local/guest-assets"
cp "$task_state/guest-assets/warden-clipboard" "$stage/warden/.local/guest-assets/warden-clipboard"
ditto "$task_state/Warden.app" "$stage/warden/.local/Warden.app"
ditto "$task_state/Warden Menu.app" "$stage/warden/.local/Warden Menu.app"
ln -s ../Warden.app/Contents/MacOS/warden-vm "$stage/warden/.local/bin/warden-vm"
ditto -c -k --sequesterRsrc --keepParent "$stage/warden" dist/warden-0.1.0-arm64.zip
shasum -a 256 dist/warden-0.1.0-arm64.zip > dist/SHA256SUMS
echo 'Built dist/warden-0.1.0-arm64.zip (recipe + launcher; no personal VM state)'
