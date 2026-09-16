#!/bin/bash
# Run INSIDE the macOS guest after Setup Assistant. Public files only.
set -euo pipefail
if [[ "$(uname -s)" != Darwin || "$(sysctl -n hw.model)" != VirtualMac* ]]; then
  echo 'Refusing: this installer must run inside the macOS VM.' >&2; exit 1
fi
if [[ "$EUID" != 0 ]]; then exec sudo /bin/bash "$0"; fi
base=http://10.77.0.1:8081
work=$(mktemp -d /private/tmp/warden-setup.XXXXXX)
trap 'rm -rf "$work"' EXIT
cd "$work"
curl -fsS "$base/SHA256SUMS" -o SHA256SUMS
for name in node.tar.gz agent-tools.tar.gz Codex.zip Claude.zip gh.zip Homebrew.pkg; do
  curl -fS "$base/$name" -o "$name"
done
shasum -a 256 -c SHA256SUMS
mkdir -p /opt/warden/installers /usr/local/libexec /usr/local/bin
for name in gh.zip Homebrew.pkg; do
  install -m 644 "$name" "/opt/warden/installers/$name"
done
awk '$2 == "gh.zip" || $2 == "Homebrew.pkg"' SHA256SUMS > /opt/warden/installers/DEVTOOLS-SHA256SUMS
curl -fsS "$base/install-guest-dev-tools.sh" -o /usr/local/libexec/warden-install-dev-tools.sh
curl -fsS "$base/install-guest-access.sh" -o /usr/local/libexec/warden-install-guest-access.sh
chmod 755 /usr/local/libexec/warden-install-guest-access.sh
curl -fsS "$base/guest-access.pub" -o /opt/warden/installers/guest-access.pub
curl -fsS "$base/warden-repo" -o /usr/local/bin/warden-repo
chmod 755 /usr/local/bin/warden-repo
chmod 755 /usr/local/libexec/warden-install-dev-tools.sh
curl -fsS "$base/ca.pem" -o /usr/local/share-warden-ca.pem
security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain /usr/local/share-warden-ca.pem
mkdir -p /opt/warden /usr/local/bin
mkdir -p /usr/local/libexec /Library/LaunchAgents
curl -fsS "$base/warden-clipboard" -o /usr/local/libexec/warden-clipboard
codesign --verify --strict /usr/local/libexec/warden-clipboard
chmod 755 /usr/local/libexec/warden-clipboard
cat > /Library/LaunchAgents/dev.warden.clipboard.plist <<'EOF'
<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>dev.warden.clipboard</string><key>ProgramArguments</key><array><string>/usr/local/libexec/warden-clipboard</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>10</integer><key>LimitLoadToSessionType</key><string>Aqua</string></dict></plist>
EOF
tar -xzf node.tar.gz -C /opt/warden
node_dir=$(find /opt/warden -maxdepth 1 -type d -name 'node-v*-darwin-arm64' | sort | tail -n 1)
ln -sfn "$node_dir/bin/node" /usr/local/bin/node
ln -sfn "$node_dir/bin/npm" /usr/local/bin/npm
ln -sfn "$node_dir/bin/npx" /usr/local/bin/npx
mkdir -p /opt/warden/agents
tar -xzf agent-tools.tar.gz -C /opt/warden/agents
cat > /usr/local/bin/codex <<'EOF'
#!/bin/sh
export NODE_EXTRA_CA_CERTS=/usr/local/share-warden-ca.pem
exec /usr/local/bin/node /opt/warden/agents/node_modules/@openai/codex/bin/codex.js "$@"
EOF
cat > /usr/local/bin/claude <<'EOF'
#!/bin/sh
export NODE_EXTRA_CA_CERTS=/usr/local/share-warden-ca.pem
export SSL_CERT_FILE=/usr/local/share-warden-ca.pem
exec /opt/warden/agents/node_modules/@anthropic-ai/claude-code-darwin-arm64/claude "$@"
EOF
chmod 755 /usr/local/bin/codex /usr/local/bin/claude
ditto -xk Codex.zip "$work/codex-app"
codex_app=$(find "$work/codex-app" -maxdepth 2 -name '*.app' -type d | head -n 1)
test -n "$codex_app"
codesign --verify --deep --strict "$codex_app"
ditto "$codex_app" "/Applications/$(basename "$codex_app")"
ditto -xk Claude.zip "$work/claude-app"
claude_app=$(find "$work/claude-app" -maxdepth 2 -name 'Claude.app' -type d | head -n 1)
test -n "$claude_app"
codesign --verify --deep --strict "$claude_app"
ditto "$claude_app" /Applications/Claude.app
cat > /etc/paths.d/warden <<'EOF'
/usr/local/bin
/opt/homebrew/bin
/opt/homebrew/sbin
EOF
cat > /etc/zshenv.warden <<'EOF'
export NODE_EXTRA_CA_CERTS=/usr/local/share-warden-ca.pem
export SSL_CERT_FILE=/usr/local/share-warden-ca.pem
export REQUESTS_CA_BUNDLE=/usr/local/share-warden-ca.pem
export DISABLE_UPDATES=1
EOF
touch /etc/zshenv
if ! grep -q '/etc/zshenv.warden' /etc/zshenv; then printf '\nsource /etc/zshenv.warden\n' >> /etc/zshenv; fi
/usr/local/bin/codex --version
/usr/local/bin/claude --version
/usr/local/libexec/warden-install-dev-tools.sh --offline
/usr/local/libexec/warden-install-guest-access.sh --offline
echo 'Warden guest tools installed. Codex and Claude are unsigned in.'
echo 'Install Apple Command Line Tools with xcode-select --install for Git and compilers.'
echo 'The clipboard receiver starts at your next guest login.'
