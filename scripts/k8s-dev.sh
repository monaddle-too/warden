#!/usr/bin/env bash
# Warden Kubernetes development loop (docs/warden-kubernetes-plan.md, appendix B).
#
#   scripts/k8s-dev.sh up            create or start the Lima VM, install Kata, gVisor RuntimeClass
#   scripts/k8s-dev.sh kubeconfig    print the kubeconfig path (export KUBECONFIG=$(…))
#   scripts/k8s-dev.sh shell [cmd]   shell into the VM
#   scripts/k8s-dev.sh build-images  build the guest base image and the warden image into k3s
#   scripts/k8s-dev.sh deploy        helm upgrade --install with the dev values
#   scripts/k8s-dev.sh test [args]   run the end-to-end suite (chat/tests/k8s) against the deployed release
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
      kubectl -n kube-system rollout status ds/kata-deploy --timeout=600s
    fi
    # Nested virtualization on Apple Silicon exposes no PMU to the VM's KVM and
    # QEMU then rejects Kata's default "cpu_features = pmu=off"; a drop-in
    # clears it (found in plan step 0).
    limactl shell "$NAME" sudo sh -c 'd=/opt/kata/share/defaults/kata-containers/runtimes/qemu/config.d; mkdir -p "$d"; printf "[hypervisor.qemu]\ncpu_features = \"\"\n" > "$d/90-warden-nested-virt.toml"'
  else
    echo "no /dev/kvm in the VM: Kata tier unavailable here (gVisor only)" >&2
  fi
  echo "export KUBECONFIG=\"$KUBECONFIG\""
}

cmd_build_images() {
  need limactl
  local rev; rev="$(git -C "$ROOT" describe --always --dirty)"
  # Frontend and the linux/arm64 binary are built on the host the same way
  # scripts/release.sh does; the image builds run inside the VM against
  # k3s's containerd, with the workspace mounted read-only at the same path.
  local pnpm=""
  if command -v pnpm >/dev/null 2>&1; then pnpm="pnpm"
  elif corepack pnpm --version >/dev/null 2>&1; then pnpm="corepack pnpm"
  else
    local cached; cached="$(ls "$HOME"/.cache/node/corepack/v1/pnpm/*/bin/pnpm.cjs 2>/dev/null | sort -V | tail -1 || true)"
    [ -n "$cached" ] || { echo "pnpm not found (install it or run corepack enable)" >&2; exit 1; }
    pnpm="node $cached"
  fi
  $pnpm --dir "$ROOT/chat/web" install --frozen-lockfile
  $pnpm --dir "$ROOT/chat/web" build
  rm -rf "$ROOT/dist/linux-arm64"; mkdir -p "$ROOT/dist/linux-arm64"
  GOPROXY=off GOFLAGS=-mod=mod CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go -C "$ROOT/chat" build -trimpath \
    -ldflags "-s -w -X warden/chat/internal/release.Revision=${rev}" -o ../dist/linux-arm64/warden ./cmd/warden
  limactl shell "$NAME" sudo bash -c "cd '$ROOT' && deploy/guest/build-base.sh --k3s --platform linux/arm64 --tag warden-guest-base:${rev} && nerdctl tag warden-guest-base:${rev} warden-guest-base:dev"
  limactl shell "$NAME" sudo bash -c "cd '$ROOT' && nerdctl build --platform linux/arm64 -f deploy/chat/Dockerfile -t warden:${rev} -t warden:dev ."
  limactl shell "$NAME" sudo nerdctl images | grep -E "^warden(-guest-base)?\s+(${rev}|dev)\b"
  # The chart pins the guest image by manifest digest; deploy reads it from here.
  limactl shell "$NAME" sudo nerdctl image inspect "warden-guest-base:${rev}" --format '{{index .RepoDigests 0}}' \
    | sed 's/.*@//' > "$ROOT/dist/guest-image-digest"
  echo "guest image digest: $(cat "$ROOT/dist/guest-image-digest")"
  # The kubelet resolves <repo>@<digest> only if containerd holds that name.
  limactl shell "$NAME" sudo ctr -n k8s.io images tag --force "docker.io/library/warden-guest-base:${rev}" \
    "docker.io/library/warden-guest-base@$(cat "$ROOT/dist/guest-image-digest")" >/dev/null
}

cmd_deploy() {
  need helm
  export KUBECONFIG; KUBECONFIG="$(kubeconfig_path)"
  local rev; rev="$(git -C "$ROOT" describe --always --dirty)"
  local digest=""; [ -f "$ROOT/dist/guest-image-digest" ] && digest="$(cat "$ROOT/dist/guest-image-digest")"
  helm upgrade --install warden "$ROOT/deploy/helm/warden" -n warden --create-namespace \
    -f "$ROOT/deploy/k8s/dev/values.yaml" --set "image.tag=${rev}" \
    ${digest:+--set "guestImage.digest=${digest}"} "$@"
}

cmd_test() {
  # The suite (docs/warden-kubernetes.md, "Development") drives the release
  # deployed by `deploy` through the edge's forwarded port and execs into
  # sandbox pods with the VM's kubeconfig; extra arguments go to go test
  # (for example -run TestKubernetes/Adversarial).
  local kubeconfig; kubeconfig="$(kubeconfig_path)"
  [ -f "$kubeconfig" ] || { echo "no kubeconfig for VM $NAME; run scripts/k8s-dev.sh up first" >&2; exit 1; }
  local port; port="$(sed -n 's/^  port: *\([0-9]*\).*/\1/p' "$ROOT/deploy/k8s/dev/values.yaml" | head -1)"
  WARDEN_K8S_KUBECONFIG="$kubeconfig" WARDEN_K8S_EDGE_URL="${WARDEN_K8S_EDGE_URL:-http://127.0.0.1:${port:-28781}}" \
    GOPROXY=off GOFLAGS=-mod=mod go -C "$ROOT/chat" test -tags k8s ./tests/k8s/ -run TestKubernetes -v -count=1 -timeout 60m "$@"
}

case "${1:-}" in
  up) shift; cmd_up "$@" ;;
  kubeconfig) kubeconfig_path ;;
  shell) shift; limactl shell "$NAME" "$@" ;;
  build-images) shift; cmd_build_images "$@" ;;
  deploy) shift; cmd_deploy "$@" ;;
  test) shift; cmd_test "$@" ;;
  down) limactl stop "$NAME" ;;
  delete) limactl delete --force "$NAME" ;;
  *) sed -n '2,11p' "$0"; exit 2 ;;
esac
