#!/bin/bash
# Guest-only installation from pinned, locally served vendor artifacts.
set -euo pipefail
[[ "$(uname -s)" == Darwin && "$(sysctl -n hw.model)" == VirtualMac* ]] || { echo 'Run this inside the Warden guest.' >&2; exit 1; }
if [[ "$EUID" != 0 ]]; then exec sudo /bin/bash "$0" "$@"; fi
mode=${1:-download}
if ! /usr/bin/shlock -f /var/run/warden-devtools.pid -p "$$"; then
  echo 'Warden developer tool installation is already running.'; exit 0
fi
trap 'rm -f /var/run/warden-devtools.pid' EXIT
assets=/opt/warden/installers
mkdir -p "$assets" /usr/local/bin /etc/paths.d
work=$(mktemp -d /private/tmp/warden-devtools.XXXXXX)
trap 'rm -rf "$work"; rm -f /var/run/warden-devtools.pid' EXIT
if [[ "$mode" == download ]]; then
  base=http://10.77.0.1:8081
  curl -fS --retry 3 "$base/warden-repo" -o "$work/warden-repo"
  install -m 755 "$work/warden-repo" /usr/local/bin/warden-repo
  curl -fS --retry 3 "$base/install-guest-access.sh" -o "$work/install-guest-access.sh"
  curl -fS --retry 3 "$base/guest-access.pub" -o "$work/guest-access.pub"
  mkdir -p /usr/local/libexec
  install -o root -g wheel -m 755 "$work/install-guest-access.sh" /usr/local/libexec/warden-install-guest-access.sh
  install -o root -g wheel -m 644 "$work/guest-access.pub" "$assets/guest-access.pub"
  for name in DEVTOOLS-SHA256SUMS gh.zip Homebrew.pkg; do
    curl -fS --retry 3 "$base/$name" -o "$work/$name"
  done
  (cd "$work" && shasum -a 256 -c DEVTOOLS-SHA256SUMS)
  for name in gh.zip Homebrew.pkg DEVTOOLS-SHA256SUMS; do
    install -m 644 "$work/$name" "$assets/$name"
  done
elif [[ "$mode" != --auto && "$mode" != --offline ]]; then
  echo 'Usage: install-guest-dev-tools.sh [--offline|--auto]' >&2; exit 1
fi

# Install the retry service as guest root, never from the rootless host mount.
if [[ "$mode" != --auto ]]; then
  mkdir -p /usr/local/libexec /Library/LaunchDaemons
  helper=/usr/local/libexec/warden-install-dev-tools.sh
  if [[ "$0" != "$helper" ]]; then install -o root -g wheel -m 755 "$0" "$helper"; fi
  chown root:wheel "$helper"
  chmod 755 "$helper"
  cat > /Library/LaunchDaemons/dev.warden.devtools.plist <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.warden.devtools</string>
<key>ProgramArguments</key><array><string>/bin/bash</string><string>/usr/local/libexec/warden-install-dev-tools.sh</string><string>--auto</string></array>
<key>StartInterval</key><integer>60</integer>
<key>StandardOutPath</key><string>/var/log/warden-devtools.log</string>
<key>StandardErrorPath</key><string>/var/log/warden-devtools.log</string>
</dict></plist>
EOF
  chown root:wheel /Library/LaunchDaemons/dev.warden.devtools.plist
  chmod 644 /Library/LaunchDaemons/dev.warden.devtools.plist
  if ! launchctl print system/dev.warden.devtools >/dev/null 2>&1; then
    launchctl bootstrap system /Library/LaunchDaemons/dev.warden.devtools.plist
  fi
fi
if [[ ! -f /etc/ssh/warden_sshd_config && -f /usr/local/libexec/warden-install-guest-access.sh && -f "$assets/guest-access.pub" ]]; then
  chown root:wheel /usr/local/libexec/warden-install-guest-access.sh
  chmod 755 /usr/local/libexec/warden-install-guest-access.sh
  /usr/local/libexec/warden-install-guest-access.sh --offline
fi
if [[ "$mode" == --auto ]] && cmp -s "$assets/DEVTOOLS-SHA256SUMS" "$assets/dev-tools-installed"; then exit 0; fi
(cd "$assets" && shasum -a 256 -c DEVTOOLS-SHA256SUMS)
ditto -xk "$assets/gh.zip" "$work/gh"
gh_binary=("$work"/gh/gh_*_macOS_arm64/bin/gh)
[[ ${#gh_binary[@]} == 1 && -f "${gh_binary[0]}" ]]
install -m 755 "${gh_binary[0]}" /usr/local/bin/gh
printf '/usr/local/bin\n/opt/homebrew/bin\n/opt/homebrew/sbin\n' > /etc/paths.d/warden
/usr/local/bin/gh --version

guest_user=$(stat -f '%Su' /dev/console)
if [[ "$guest_user" == root || "$guest_user" == loginwindow || "$guest_user" == _* ]] || ! id "$guest_user" >/dev/null 2>&1; then
  echo 'GitHub CLI installed. Homebrew is waiting for a logged-in guest user.'
  exit 0
fi
if ! /usr/bin/xcrun --find git >/dev/null 2>&1; then
  echo 'GitHub CLI installed. Install Apple Command Line Tools in the guest with: xcode-select --install'
  echo 'Homebrew will finish automatically afterward (within one minute).'
  exit 0
fi
if [[ ! -x /opt/homebrew/bin/brew ]]; then
  /usr/sbin/pkgutil --check-signature "$assets/Homebrew.pkg"
  /usr/sbin/installer -pkg "$assets/Homebrew.pkg" -target /
fi
/usr/bin/sudo -H -u "$guest_user" /opt/homebrew/bin/brew --version
cp "$assets/DEVTOOLS-SHA256SUMS" "$assets/dev-tools-installed"
echo 'GitHub CLI and Homebrew are ready. Open a new guest Terminal to refresh PATH.'
