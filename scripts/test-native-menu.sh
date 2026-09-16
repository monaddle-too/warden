#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/warden-menu-test.XXXXXX")
trap 'rm -rf "$test_dir"' EXIT
swiftc native/Sources/WardenVM/StatusMenu.swift tests/native-menu-check.swift \
  -o "$test_dir/native-menu-check" -framework AppKit
"$test_dir/native-menu-check" "$@"
