#!/usr/bin/env bash
# Warden Kubernetes development loop (docs/warden-kubernetes-plan.md, appendix B).
#
#   scripts/k8s-dev.sh up            create or start the Lima VM, install Kata, gVisor RuntimeClass
#   scripts/k8s-dev.sh kubeconfig    print the kubeconfig path (export KUBECONFIG=$(…))
#   scripts/k8s-dev.sh shell [cmd]   shell into the VM
#   scripts/k8s-dev.sh build-images  build the guest base image and the warden image into k3s
#   scripts/k8s-dev.sh deploy        helm upgrade --install with the dev values
#   scripts/k8s-dev.sh down          stop the VM (state kept); `delete` removes it
set -euo pipefail

NAME="${WARDEN_K8S_VM:-warden-k8s}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
KATA_VERSION="${KATA_VERSION:-4.2.0}"
KATA_CHART="https://github.com/kata-containers/kata-containers/releases/download/${KATA_VERSION}/kata-deploy-${KATA_VERSION}.tgz"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing $1 (brew install lima kubectl helm)" >&2; exit 1; }; }

kubeconfig_path() {
  limactl list "$NAME" --format '{{.Dir}}/copied-from-guest/kubeconfig.yaml' 2>/dev/null
}

cmd_up() {
  need limactl; need kubectl; need helm
  if ! limactl list --format '{{.Name}}' | grep -qx "$NAME"; then
    limactl create --name "$NAME" --tty=false "$ROOT/deploy/k8s/dev/lima.yaml"
  fi
  if [ "$(limactl list "$NAME" --format '{{.Status}}')" != Running ]; then
    limactl start --tty=false "$NAME"
  fi
  export KUBECONFIG; KUBECONFIG="$(kubeconfig_path)"
  for _ in $(seq 1 60); do kubectl get nodes >/dev/null 2>&1 && break; sleep 2; done
  kubectl apply -f - <<'RC'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
RC
  if limactl shell "$NAME" test -e /dev/kvm; then
    if ! helm -n kube-system status kata-deploy >/dev/null 2>&1; then
      helm install kata-deploy "$KATA_CHART" -n kube-system --set k8sDistribution=k3s \
        --set 'shims.disableAll=true' --set 'shims.qemu.enabled=true' --set 'defaultShim.arm64=qemu' --set 'defaultShim.amd64=qemu'
    fi
  else
    echo "no /dev/kvm in the VM: Kata tier unavailable here (gVisor only)" >&2
  fi
  echo "export KUBECONFIG=\"$KUBECONFIG\""
}

cmd_build_images() {
  need limactl
  local rev; rev="$(git -C "$ROOT" describe --always --dirty)"
  # The workspace is mounted read-only at the same path inside the VM.
  limactl shell "$NAME" sudo nerdctl build --platform linux/arm64 -f "$ROOT/deploy/guest/Dockerfile.base" \
    -t "warden-guest-base:${rev}" "$ROOT/deploy/guest"
  limactl shell "$NAME" sudo nerdctl build --platform linux/arm64 -f "$ROOT/deploy/chat/Dockerfile" \
    -t "warden:${rev}" "$ROOT"
  limactl shell "$NAME" sudo nerdctl images | grep -E "warden(-guest-base)?\s+${rev}"
}

cmd_deploy() {
  need helm
  export KUBECONFIG; KUBECONFIG="$(kubeconfig_path)"
  local rev; rev="$(git -C "$ROOT" describe --always --dirty)"
  helm upgrade --install warden "$ROOT/deploy/helm/warden" -n warden --create-namespace \
    -f "$ROOT/deploy/k8s/dev/values.yaml" --set "image.tag=${rev}" --set "guestImage.tag=${rev}" "$@"
}

case "${1:-}" in
  up) shift; cmd_up "$@" ;;
  kubeconfig) kubeconfig_path ;;
  shell) shift; limactl shell "$NAME" "$@" ;;
  build-images) shift; cmd_build_images "$@" ;;
  deploy) shift; cmd_deploy "$@" ;;
  down) limactl stop "$NAME" ;;
  delete) limactl delete --force "$NAME" ;;
  *) sed -n '2,10p' "$0"; exit 2 ;;
esac
