#!/bin/bash
# Run only in the trusted proxy VM BEFORE macOS starts. Exercises the actual
# nftables/mitmproxy path using an attacker network namespace on the private NIC.
set -euo pipefail
[[ ${1:-} == --disposable ]] || { echo 'Requires --disposable and a dedicated validation appliance.' >&2; exit 64; }
test "$(hostname)" = warden-proxy
test "$(id -u)" = 0
if test -f /var/lib/misc/dnsmasq.leases && grep -qi '02:57:41:00:00:03' /var/lib/misc/dnsmasq.leases; then
  echo 'Refusing to rewire an appliance with a macOS guest lease.' >&2; exit 1
fi
ip link show lan0 > /dev/null
test ! -e /run/warden-network-test
touch /run/warden-network-test
systemctl stop warden-sync.timer
cleanup() {
  ip netns del warden-attacker 2>/dev/null || true
  ip link del lan0 2>/dev/null || true
  ip link set vzlan0 name lan0 2>/dev/null || true
  ip addr replace 10.77.0.1/24 dev lan0
  ip link set lan0 up
  systemctl restart dnsmasq warden-proxy.service warden-assets.service
  systemctl start warden-sync.timer
  rm -f /run/warden-network-test
}
trap cleanup EXIT
ip link set lan0 down
ip addr flush dev lan0
ip link set lan0 name vzlan0
ip netns add warden-attacker
ip link add lan0 type veth peer name attacker0
ip link set attacker0 netns warden-attacker
ip addr add 10.77.0.1/24 dev lan0
ip link set lan0 up
ip -n warden-attacker link set lo up
ip -n warden-attacker addr add 10.77.0.2/24 dev attacker0
ip -n warden-attacker link set attacker0 up
ip -n warden-attacker route add default via 10.77.0.1
mkdir -p /etc/netns/warden-attacker
printf 'nameserver 10.77.0.1\n' > /etc/netns/warden-attacker/resolv.conf
systemctl restart dnsmasq
systemctl restart warden-telemetry.service
python3 - <<'PY'
import socket,time
for attempt in range(100):
    try:
        with socket.create_connection(('127.0.0.1',8080),timeout=.2): break
    except OSError: time.sleep(.1)
else: raise SystemExit('proxy did not start listening')
PY
count=0
expect_status() {
  local expected=$1 name=$2
  shift 2
  local actual
  actual=$(ip netns exec warden-attacker curl --noproxy '*' --cacert /var/lib/warden/mitmproxy/mitmproxy-ca-cert.pem --max-time 15 -sS -o /run/warden-test-response -w '%{http_code}' "$@")
  if [[ "$actual" != "$expected" ]]; then
    echo "FAIL $name: expected $expected, got $actual"
    head -c 1000 /run/warden-test-response
    exit 1
  fi
  echo "PASS $name ($actual)"
  count=$((count+1))
}
expect_status 200 public_https https://example.com/
expect_status 428 github_rest_needs_approval https://api.github.com/repos/openai/codex
expect_status 403 github_website_denied https://github.com/
expect_status 403 github_graphql_denied https://api.github.com/graphql
expect_status 403 sni_host_mismatch https://example.com/ -H 'Host: api.github.com'
expect_status 200 public_bootstrap_ca http://10.77.0.1:8081/ca.pem
for destination in '140.82.116.4 22' '140.82.116.4 9418' '192.168.64.1 18765'; do
  read -r host port <<< "$destination"
  if ip netns exec warden-attacker timeout 2 bash -c 'exec 3<>/dev/tcp/$1/$2' -- "$host" "$port" 2>/dev/null; then
    echo "FAIL opaque/host connection escaped: $destination"; exit 1
  fi
  echo "PASS blocked TCP $destination"; count=$((count+1))
done
systemctl stop warden-proxy.service
if ip netns exec warden-attacker curl --noproxy '*' --max-time 3 -ks https://example.com/ > /dev/null; then
  echo 'FAIL proxy shutdown opened forwarding'; exit 1
fi
echo 'PASS fail closed with proxy stopped'; count=$((count+1))
printf '{"passed":%s,"failed":0,"method":"Linux attacker namespace through production nftables and mitmproxy"}\n' "$count" > /var/lib/warden/network-test-results.json
cat /var/lib/warden/network-test-results.json
