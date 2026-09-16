#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
swift build --package-path native -c release
task_state=${WARDEN_STATE:-$PWD/.local}
mkdir -p "$task_state/guest-assets"
cp native/.build/release/warden-clipboard "$task_state/guest-assets/warden-clipboard"
codesign --force --sign - "$task_state/guest-assets/warden-clipboard"
mkdir -p "$task_state/bin" "$task_state/Warden.app/Contents/MacOS"
cp native/.build/release/warden-vm "$task_state/Warden.app/Contents/MacOS/warden-vm.new"
mv -f "$task_state/Warden.app/Contents/MacOS/warden-vm.new" "$task_state/Warden.app/Contents/MacOS/warden-vm"
cp native/Info.plist "$task_state/Warden.app/Contents/Info.plist"
codesign --force --sign - --entitlements native/warden.entitlements "$task_state/Warden.app"
mkdir -p "$task_state/Warden Menu.app/Contents/MacOS"
cp native/.build/release/warden-vm "$task_state/Warden Menu.app/Contents/MacOS/warden-vm.new"
mv -f "$task_state/Warden Menu.app/Contents/MacOS/warden-vm.new" "$task_state/Warden Menu.app/Contents/MacOS/warden-vm"
cp native/MenuInfo.plist "$task_state/Warden Menu.app/Contents/Info.plist"
codesign --force --sign - "$task_state/Warden Menu.app"
ln -sfn ../Warden.app/Contents/MacOS/warden-vm "$task_state/bin/warden-vm"
"$task_state/bin/warden-vm" doctor
