#!/usr/bin/env bash
# Step 0 spike runner: applies the manifests, then probes the NetworkPolicy
# behaviour, source-IP anti-spoofing, ConfigMap refresh and pod start times
# per tier. Needs KUBECONFIG pointing at the dev cluster. Prints a table;
# nothing here is trusted by Warden itself (the verifier's proofs are
# canaries and control-plane facts, decision 12).
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
NS=warden-spike; SNS=warden-spike-sandboxes
TIERS="${TIERS:-gvisor kata}"

k() { kubectl "$@"; }
ip_of() { k -n "$1" get pod "$2" -o jsonpath='{.status.podIP}'; }
# probe <ns> <pod> <host> <port> → "open" | "closed" | "timeout"
probe() {
  local out
  out=$(k -n "$1" exec "$2" -- sh -c "nc -z -w 3 $3 $4 >/dev/null 2>&1 && echo open || echo blocked" 2>/dev/null)
  echo "${out:-exec-failed}"
}
row() { printf '%-34s %-10s %s\n' "$1" "$2" "$3"; }
expect() { # expect <label> <got> <want>
  if [ "$2" = "$3" ]; then row "$1" "$2" "ok"; else row "$1" "$2" "FAIL (want $3)"; FAILED=1; fi
}
FAILED=0

echo "== apply"
k apply -f "$HERE/00-namespaces.yaml" -f "$HERE/10-networkpolicies.yaml" -f "$HERE/20-core.yaml" >/dev/null
k apply -f "$HERE/30-sandboxes.yaml" >/dev/null
for p in gateway decoy runner outsider; do k -n $NS wait --for=condition=Ready pod/$p --timeout=180s >/dev/null || echo "not ready: $p"; done
for t in $TIERS; do k -n $SNS wait --for=condition=Ready pod/sb-$t --timeout=300s >/dev/null || echo "not ready: sb-$t"; done
k -n $SNS wait --for=condition=Ready pod/sb-gvisor-denied --timeout=300s >/dev/null || echo "not ready: sb-gvisor-denied"

GW=$(ip_of $NS gateway); DECOY=$(ip_of $NS decoy); RUNNER=$(ip_of $NS runner)
API=$(k get svc kubernetes -n default -o jsonpath='{.spec.clusterIP}')
DNS=$(k get svc kube-dns -n kube-system -o jsonpath='{.spec.clusterIP}')
NODE=$(k get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
echo "gateway=$GW decoy=$DECOY runner=$RUNNER api=$API dns=$DNS node=$NODE"

echo; echo "== runtime classes actually in use"
for t in $TIERS; do
  printf '%-10s runtimeClass=%s  ' "$t" "$(k -n $SNS get pod sb-$t -o jsonpath='{.spec.runtimeClassName}')"
  k -n $SNS exec sb-$t -- sh -c 'uname -r; (dmesg 2>/dev/null | head -1) || true' 2>/dev/null | tr '\n' ' '; echo
done

echo; echo "== egress from a labelled sandbox (want: only the gateway)"
for t in $TIERS; do
  expect "$t -> gateway:7000"      "$(probe $SNS sb-$t $GW 7000)"    open
  expect "$t -> decoy:7000"        "$(probe $SNS sb-$t $DECOY 7000)" blocked
  expect "$t -> gateway:80"        "$(probe $SNS sb-$t $GW 80)"      blocked
  expect "$t -> apiserver:443"     "$(probe $SNS sb-$t $API 443)"    blocked
  expect "$t -> kube-dns:53"       "$(probe $SNS sb-$t $DNS 53)"     blocked
  expect "$t -> node:22"           "$(probe $SNS sb-$t $NODE 22)"    blocked
  expect "$t -> 1.1.1.1:443"       "$(probe $SNS sb-$t 1.1.1.1 443)" blocked
  expect "$t -> 169.254.169.254:80" "$(probe $SNS sb-$t 169.254.169.254 80)" blocked
done
echo; echo "== egress from an unlabelled sandbox (want: nothing)"
expect "denied -> gateway:7000" "$(probe $SNS sb-gvisor-denied $GW 7000)" blocked

echo; echo "== label flip on the running gvisor sandbox (grant/deny without restart)"
k -n $SNS label pod sb-gvisor warden.monaddle.com/egress- --overwrite >/dev/null
sleep 3
expect "unlabelled -> gateway:7000" "$(probe $SNS sb-gvisor $GW 7000)" blocked
k -n $SNS label pod sb-gvisor warden.monaddle.com/egress=gateway --overwrite >/dev/null
sleep 3
expect "relabelled -> gateway:7000" "$(probe $SNS sb-gvisor $GW 7000)" open

echo; echo "== ingress to sandboxes (want: runner only)"
for t in $TIERS; do
  SB=$(ip_of $SNS sb-$t)
  expect "runner -> $t:8080"   "$(probe $NS runner $SB 8080)"   open
  expect "outsider -> $t:8080" "$(probe $NS outsider $SB 8080)" blocked
  expect "gateway -> $t:8080"  "$(probe $NS gateway $SB 8080)"  blocked
done

echo; echo "== source-IP anti-spoofing (a sandbox sending with the runner's IP; want: blocked)"
for t in $TIERS; do
  SBIP=$(ip_of $SNS sb-$t)
  out=$(k -n $SNS exec sb-$t -- sh -c "ip addr add $RUNNER/32 dev eth0 2>&1; ip route replace default dev eth0 2>/dev/null; nc -z -w 3 -s $RUNNER $DECOY 7000 >/dev/null 2>&1 && echo open || echo blocked; ip addr del $RUNNER/32 dev eth0 2>/dev/null; true" 2>&1 | tail -1)
  row "$t spoof runner -> decoy:7000" "$out" "$([ "$out" = blocked ] && echo ok || echo 'NOTE: spoofing not filtered by this CNI')"
done

echo; echo "== ConfigMap refresh inside a running pod (decision 9)"
k -n $SNS delete cm spike-trust --ignore-not-found >/dev/null
k -n $SNS create cm spike-trust --from-literal=ca-certificates.crt=v1 >/dev/null
for t in $TIERS; do
  k -n $SNS delete pod cm-$t --ignore-not-found --wait=true >/dev/null 2>&1
  k -n $SNS apply -f - >/dev/null <<POD
apiVersion: v1
kind: Pod
metadata: {name: cm-$t, namespace: $SNS}
spec:
  runtimeClassName: $([ $t = kata ] && echo kata-qemu || echo $t)
  automountServiceAccountToken: false
  volumes: [{name: trust, configMap: {name: spike-trust}}]
  containers:
  - name: guest
    image: docker.io/library/busybox:1.37
    command: ["sleep", "infinity"]
    volumeMounts: [{name: trust, mountPath: /opt/warden/trust}]
POD
  k -n $SNS wait --for=condition=Ready pod/cm-$t --timeout=300s >/dev/null || echo "not ready: cm-$t"
done
k -n $SNS create cm spike-trust --from-literal=ca-certificates.crt=v2 --dry-run=client -o yaml | k apply -f - >/dev/null
for t in $TIERS; do
  got=""; for i in $(seq 1 40); do got=$(k -n $SNS exec cm-$t -- cat /opt/warden/trust/ca-certificates.crt 2>/dev/null); [ "$got" = v2 ] && break; sleep 3; done
  expect "$t configmap refresh (${i}x3s)" "$got" v2
done

echo; echo "== pod start time per tier (create → Ready, image cached), 3 samples"
for t in $TIERS; do
  for i in 1 2 3; do
    k -n $SNS delete pod time-$t --ignore-not-found --wait=true >/dev/null 2>&1
    s=$(date +%s.%N)
    k -n $SNS apply -f - >/dev/null <<POD
apiVersion: v1
kind: Pod
metadata: {name: time-$t, namespace: $SNS}
spec:
  runtimeClassName: $([ $t = kata ] && echo kata-qemu || echo $t)
  automountServiceAccountToken: false
  containers: [{name: guest, image: docker.io/library/busybox:1.37, command: ["sleep", "infinity"]}]
POD
    k -n $SNS wait --for=condition=Ready pod/time-$t --timeout=300s >/dev/null
    e=$(date +%s.%N); printf '%-10s sample %d: %.1fs\n' "$t" "$i" "$(echo "$e - $s" | bc)"
  done
  k -n $SNS delete pod time-$t --wait=false >/dev/null 2>&1
done

echo; [ $FAILED = 0 ] && echo "SPIKE: all expectations met" || echo "SPIKE: some expectations FAILED (see above)"
