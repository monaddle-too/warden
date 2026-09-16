#!/bin/bash
# Install for the logged-in guest user; no root or host clipboard access needed.
set -euo pipefail
[[ "$(uname -s)" == Darwin && "$(sysctl -n hw.model)" == VirtualMac* ]] || { echo 'Run this inside the Warden guest.' >&2; exit 1; }
[[ "$EUID" != 0 ]] || { echo 'Run as your guest login user, without sudo.' >&2; exit 1; }
destination="$HOME/Library/Application Support/Warden"
mkdir -p "$destination" "$HOME/Library/LaunchAgents"
curl -fsS --max-time 30 http://10.77.0.1:8081/warden-clipboard -o "$destination/warden-clipboard.new"
codesign --verify --strict "$destination/warden-clipboard.new"
chmod 755 "$destination/warden-clipboard.new"
mv "$destination/warden-clipboard.new" "$destination/warden-clipboard"
plist="$HOME/Library/LaunchAgents/dev.warden.clipboard.plist"
plutil -create xml1 "$plist"
plutil -insert Label -string dev.warden.clipboard "$plist"
plutil -insert ProgramArguments -array "$plist"
plutil -insert ProgramArguments.0 -string "$destination/warden-clipboard" "$plist"
plutil -insert RunAtLoad -bool YES "$plist"
plutil -insert KeepAlive -bool YES "$plist"
plutil -insert ThrottleInterval -integer 10 "$plist"
plutil -insert LimitLoadToSessionType -string Aqua "$plist"
chmod 600 "$plist"
launchctl bootout "gui/$UID/dev.warden.clipboard" 2>/dev/null || true
launchctl bootstrap "gui/$UID" "$HOME/Library/LaunchAgents/dev.warden.clipboard.plist"
echo 'Guest clipboard receiver installed. Use Paste from Host (Command-Shift-V) in the Warden window.'
