#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/warden-native-test.XXXXXX")
trap 'rm -rf "$test_dir"' EXIT
swiftc native/Sources/WardenVM/HostPaste.swift tests/native-paste-check.swift \
  -o "$test_dir/native-paste-check" -framework AppKit -framework Virtualization
"$test_dir/native-paste-check"
