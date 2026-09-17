#!/usr/bin/env bash
# Warden on a GKE Autopilot test cluster with Google sign-in and public
# previews (docs/warden-kubernetes.md, "A real cluster: GKE Autopilot with
# public previews"). Settings come from deploy/k8s/gke/env (see env.example).
#
#   scripts/k8s-gke.sh up            APIs, the Autopilot cluster, the Cloud DNS zone, Artifact Registry,
#                                    ingress-nginx, cert-manager with Workload Identity, the issuers
#   scripts/k8s-gke.sh dns           name servers to delegate the zone to; A records to the ingress
#   scripts/k8s-gke.sh build-images  build linux/amd64 images in the dev VM (emulated) and push them
#   scripts/k8s-gke.sh secrets       copy the three provider login Secrets from the dev cluster
#   scripts/k8s-gke.sh deploy [args] helm upgrade --install with deploy/k8s/gke/values.yaml
#   scripts/k8s-gke.sh status        nodes, pods, certificate, ingress, delegation
#   scripts/k8s-gke.sh park|unpark   scale everything to zero (disks and the ingress stay) and back
#   scripts/k8s-gke.sh down          delete the cluster and its disks (zone, images, delegation stay)
#   scripts/k8s-gke.sh delete        remove everything `up` created
#
# The kubeconfig is dist/gke/kubeconfig, not ~/.kube/config:
#   export KUBECONFIG="$(scripts/k8s-gke.sh kubeconfig)"
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="${WARDEN_GKE_ENV:-$ROOT/deploy/k8s/gke/env}"
DEV_VM="${WARDEN_K8S_VM:-warden-k8s}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing $1 ($2)" >&2; exit 1; }; }

load_env() {
  [ -f "$ENV_FILE" ] || { echo "no $ENV_FILE; copy deploy/k8s/gke/env.example and fill it in" >&2; exit 1; }
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  for v in WARDEN_GKE_PROJECT WARDEN_GKE_REGION WARDEN_GKE_CLUSTER WARDEN_GKE_DOMAIN WARDEN_GKE_OWNER; do
    [ -n "${!v:-}" ] || { echo "$v is not set in $ENV_FILE" >&2; exit 1; }
  done
  PROJECT="$WARDEN_GKE_PROJECT"; REGION="$WARDEN_GKE_REGION"; CLUSTER="$WARDEN_GKE_CLUSTER"
  DOMAIN="${WARDEN_GKE_DOMAIN%.}"; OWNER="$WARDEN_GKE_OWNER"
  ZONE="${WARDEN_GKE_ZONE:-warden}"
  REGISTRY="${WARDEN_GKE_REGISTRY:-${REGION}-docker.pkg.dev/${PROJECT}/warden}"
  GSA="warden-dns01@${PROJECT}.iam.gserviceaccount.com"
  case "${WARDEN_GKE_ACME:-staging}" in
    staging) ISSUER=letsencrypt-staging ;;
    production) ISSUER=letsencrypt ;;
    *) echo "WARDEN_GKE_ACME must be staging or production" >&2; exit 1 ;;
  esac
  mkdir -p "$ROOT/dist/gke"
  export KUBECONFIG="$ROOT/dist/gke/kubeconfig"
  # kubectl authenticates to GKE through gke-gcloud-auth-plugin, which the
  # Homebrew cask installs beside gcloud without linking it onto PATH.
  local sdk; sdk="$(gcloud info --format='value(installation.sdk_root)' 2>/dev/null || true)"
  [ -z "$sdk" ] || export PATH="$PATH:$sdk/bin"
}

gcloud_() { gcloud --project "$PROJECT" --quiet "$@"; }

cmd_up() {
  load_env
  need gcloud "brew install --cask google-cloud-sdk; gcloud auth login"
  need gke-gcloud-auth-plugin "gcloud components install gke-gcloud-auth-plugin"
  need kubectl "brew install kubectl"; need helm "brew install helm"
  gcloud_ services enable container.googleapis.com dns.googleapis.com artifactregistry.googleapis.com compute.googleapis.com

  # The cluster: Autopilot (one is free of the management fee under the GKE
  # free tier), nodes with public addresses so image pulls, the providers
  # and Let's Encrypt need no Cloud NAT (about $32 a month). Autopilot
  # brings Dataplane V2 (NetworkPolicy enforced), Workload Identity and the
  # gvisor RuntimeClass.
  if ! gcloud_ container clusters describe "$CLUSTER" --region "$REGION" >/dev/null 2>&1; then
    gcloud_ container clusters create-auto "$CLUSTER" --region "$REGION" --release-channel regular --no-enable-private-nodes
  fi
  gcloud_ container clusters get-credentials "$CLUSTER" --region "$REGION"

  # The zone for the app host and the wildcard preview host; `dns` prints
  # the name servers to delegate it to.
  gcloud_ dns managed-zones describe "$ZONE" >/dev/null 2>&1 \
    || gcloud_ dns managed-zones create "$ZONE" --dns-name "${DOMAIN}." --description "Warden: ${DOMAIN} and *.${DOMAIN}"

  # Images built by build-images; the nodes pull them with the default
  # compute service account, which new projects grant nothing by default.
  gcloud_ artifacts repositories describe warden --location "$REGION" >/dev/null 2>&1 \
    || gcloud_ artifacts repositories create warden --repository-format docker --location "$REGION" --description "Warden images"
  local number; number="$(gcloud_ projects describe "$PROJECT" --format 'value(projectNumber)')"
  gcloud_ artifacts repositories add-iam-policy-binding warden --location "$REGION" \
    --member "serviceAccount:${number}-compute@developer.gserviceaccount.com" --role roles/artifactregistry.reader >/dev/null

  # cert-manager solves DNS-01 as this Google service account through
  # Workload Identity (cert-manager's Cloud DNS guide): dns.admin on the
  # project, and the cert-manager ServiceAccount may impersonate it.
  gcloud_ iam service-accounts describe "$GSA" >/dev/null 2>&1 \
    || gcloud_ iam service-accounts create warden-dns01 --display-name "Warden cert-manager DNS-01"
  gcloud_ projects add-iam-policy-binding "$PROJECT" --member "serviceAccount:${GSA}" --role roles/dns.admin --condition=None >/dev/null
  gcloud_ iam service-accounts add-iam-policy-binding "$GSA" --role roles/iam.workloadIdentityUser \
    --member "serviceAccount:${PROJECT}.svc.id.goog[cert-manager/cert-manager]" >/dev/null

  helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx >/dev/null 2>&1 || true
  helm repo add jetstack https://charts.jetstack.io >/dev/null 2>&1 || true
  helm repo update >/dev/null
  # The admission webhook is off: GKE's control plane reaches node ports
  # 443 and 10250 only, and the webhook (8443) would need a firewall rule;
  # it only validates Ingress syntax. cert-manager's webhook listens on
  # 10250 for exactly that reason.
  helm upgrade --install ingress-nginx ingress-nginx/ingress-nginx -n ingress-nginx --create-namespace \
    --set controller.admissionWebhooks.enabled=false --wait --timeout 10m
  # Leader election in cert-manager's own namespace: Autopilot forbids the
  # leases cert-manager would otherwise keep in kube-system (cainjector then
  # never injects the webhook CA and the startup check fails).
  helm upgrade --install cert-manager jetstack/cert-manager -n cert-manager --create-namespace \
    --set crds.enabled=true --set global.leaderElection.namespace=cert-manager \
    --set "serviceAccount.annotations.iam\.gke\.io/gcp-service-account=${GSA}" --wait --timeout 10m
  sed -e "s/__PROJECT__/${PROJECT}/g" -e "s/__EMAIL__/${OWNER}/g" "$ROOT/deploy/k8s/gke/cluster-issuer.yaml" | kubectl apply -f -
  cmd_dns
  echo "export KUBECONFIG=\"$KUBECONFIG\""
}

ingress_ip() {
  kubectl -n ingress-nginx get svc ingress-nginx-controller -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true
}

record() { # name ip: create or update an A record in the zone
  local name="$1" ip="$2" current
  current="$(gcloud_ dns record-sets describe "$name" --type A --zone "$ZONE" --format 'value(rrdatas[0])' 2>/dev/null || true)"
  if [ -z "$current" ]; then
    gcloud_ dns record-sets create "$name" --type A --ttl 300 --rrdatas "$ip" --zone "$ZONE" >/dev/null
  elif [ "$current" != "$ip" ]; then
    gcloud_ dns record-sets update "$name" --type A --ttl 300 --rrdatas "$ip" --zone "$ZONE" >/dev/null
  fi
  echo "$name A $ip"
}

cmd_dns() {
  load_env
  echo "Delegate ${DOMAIN} to the zone's name servers: at the registrar or DNS host of the parent"
  echo "domain, add one NS record for '${DOMAIN%%.*}' per line below (once; they do not change):"
  gcloud_ dns managed-zones describe "$ZONE" --format 'value(nameServers)' | tr ';' '\n' | sed 's/^/  /'
  local ip; ip="$(ingress_ip)"
  if [ -z "$ip" ]; then
    echo "the ingress has no external address yet (kubectl -n ingress-nginx get svc); run 'dns' again later" >&2
    return 1
  fi
  record "${DOMAIN}." "$ip"
  record "*.${DOMAIN}." "$ip"
  if command -v dig >/dev/null 2>&1; then
    local ns; ns="$(dig +short NS "$DOMAIN" @8.8.8.8 2>/dev/null | head -1 || true)"
    case "$ns" in
      *googledomains.com.|*google.com.) echo "delegation: in place (${ns})" ;;
      "") echo "delegation: not visible yet (the parent has no NS records for ${DOMAIN}, or they have not propagated)" ;;
      *) echo "delegation: ${DOMAIN} resolves through ${ns}, not the Cloud DNS zone" ;;
    esac
  fi
}

cmd_build_images() {
  load_env
  need limactl "brew install lima"; need gcloud "brew install --cask google-cloud-sdk"
  [ "$(limactl list "$DEV_VM" --format '{{.Status}}' 2>/dev/null)" = Running ] \
    || { echo "the dev VM $DEV_VM is not running (scripts/k8s-dev.sh up); it builds the amd64 images under emulation" >&2; exit 1; }
  local rev; rev="$(git -C "$ROOT" describe --always --dirty)"
  # The web UI and the linux/amd64 binary on the Mac, as scripts/k8s-dev.sh
  # does for arm64; the images in the VM against k3s's containerd, where
  # buildkitd runs, with qemu-user-static for the amd64 RUN steps.
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
  rm -rf "$ROOT/dist/linux-amd64"; mkdir -p "$ROOT/dist/linux-amd64"
  GOPROXY=off GOFLAGS=-mod=mod CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C "$ROOT/chat" build -trimpath \
    -ldflags "-s -w -X warden/chat/internal/release.Revision=${rev}" -o ../dist/linux-amd64/warden ./cmd/warden
  # The amd64 RUN steps run under QEMU user emulation: the binfmt image's
  # statically linked QEMU (v10.2), registered with the fix-binary flag so
  # it works inside the build containers. Ubuntu 24.04's qemu-user-static
  # (8.2) segfaults in Python's and the runtimes' installers; a distro
  # registration is replaced.
  # shellcheck disable=SC2016
  limactl shell "$DEV_VM" sudo bash -c '
    f=/proc/sys/fs/binfmt_misc/qemu-x86_64
    if ! grep -q "^interpreter /usr/bin/qemu-x86_64$" "$f" 2>/dev/null; then
      [ ! -e "$f" ] || echo -1 > "$f"
      nerdctl --namespace k8s.io --address /run/k3s/containerd/containerd.sock run --net none --privileged --rm \
        docker.io/tonistiigi/binfmt@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0 --install amd64 >/dev/null
    fi'
  local -a nerdctl=(sudo nerdctl --namespace k8s.io --address /run/k3s/containerd/containerd.sock)
  # A short-lived access token of the signed-in gcloud account, as the
  # registry's docker credential helper would present it.
  gcloud_ auth print-access-token | limactl shell "$DEV_VM" "${nerdctl[@]}" login -u oauth2accesstoken --password-stdin "${REGION}-docker.pkg.dev"
  limactl shell "$DEV_VM" sudo bash -c "cd '$ROOT' && deploy/guest/build-base.sh --k3s --platform linux/amd64 --tag '${REGISTRY}/warden-guest-base:${rev}'"
  limactl shell "$DEV_VM" sudo bash -c "cd '$ROOT' && nerdctl --namespace k8s.io --address /run/k3s/containerd/containerd.sock build --platform linux/amd64 -f deploy/chat/Dockerfile -t '${REGISTRY}/warden:${rev}' ."
  limactl shell "$DEV_VM" "${nerdctl[@]}" push "${REGISTRY}/warden-guest-base:${rev}"
  limactl shell "$DEV_VM" "${nerdctl[@]}" push "${REGISTRY}/warden:${rev}"
  # The chart pins the guest image by the manifest digest the registry
  # holds; deploy reads it from here, with the revision it belongs to.
  gcloud_ artifacts docker images describe "${REGISTRY}/warden-guest-base:${rev}" --format 'value(image_summary.digest)' > "$ROOT/dist/gke/guest-image-digest"
  echo "$rev" > "$ROOT/dist/gke/image-rev"
  echo "pushed ${REGISTRY}/warden:${rev} and ${REGISTRY}/warden-guest-base:${rev}"
  echo "guest image digest: $(cat "$ROOT/dist/gke/guest-image-digest")"
}

cmd_secrets() {
  load_env
  need limactl "brew install lima"
  local dev; dev="$(limactl list "$DEV_VM" --format '{{.Dir}}/copied-from-guest/kubeconfig.yaml' 2>/dev/null)"
  [ -f "$dev" ] || { echo "no kubeconfig for the dev VM $DEV_VM (scripts/k8s-dev.sh up)" >&2; exit 1; }
  kubectl create namespace warden --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  # The same login Secrets the dev cluster holds (docs/warden-kubernetes.md,
  # "The release namespace and the provider Secrets"), copied object by
  # object without the dev cluster's metadata.
  for s in warden-codex-login warden-claude-login warden-github-login; do
    KUBECONFIG="$dev" kubectl -n warden get secret "$s" -o json | python3 -c '
import json, sys
o = json.load(sys.stdin)
o["metadata"] = {"name": o["metadata"]["name"], "namespace": o["metadata"]["namespace"]}
json.dump(o, sys.stdout)' | kubectl -n warden apply -f - >/dev/null
    echo "copied $s"
  done
}

cmd_deploy() {
  load_env
  need helm "brew install helm"
  [ -n "${WARDEN_GKE_CLIENT_ID:-}" ] || { echo "WARDEN_GKE_CLIENT_ID is not set in $ENV_FILE (the Google sign-in web client)" >&2; exit 1; }
  [ -f "$ROOT/dist/gke/guest-image-digest" ] && [ -f "$ROOT/dist/gke/image-rev" ] \
    || { echo "no images recorded in dist/gke; run scripts/k8s-gke.sh build-images first" >&2; exit 1; }
  local rev digest; rev="$(cat "$ROOT/dist/gke/image-rev")"; digest="$(cat "$ROOT/dist/gke/guest-image-digest")"
  helm upgrade --install warden "$ROOT/deploy/helm/warden" -n warden --create-namespace \
    -f "$ROOT/deploy/k8s/gke/values.yaml" \
    --set "image.repository=${REGISTRY}/warden" --set "image.tag=${rev}" \
    --set "guestImage.repository=${REGISTRY}/warden-guest-base" --set "guestImage.digest=${digest}" \
    --set "auth.publicURL=https://${DOMAIN}" --set "auth.google.signInClientID=${WARDEN_GKE_CLIENT_ID}" \
    --set "auth.google.owners={${OWNER}}" --set "previews.hostSuffix=${DOMAIN}" \
    --set "previews.ingress.annotations.cert-manager\.io/cluster-issuer=${ISSUER}" "$@"
  echo "open https://${DOMAIN}/ and sign in as ${OWNER} once the certificate is ready (scripts/k8s-gke.sh status)"
}

cmd_status() {
  load_env
  kubectl get nodes -o wide 2>/dev/null | sed 's/^/node: /' || true
  kubectl -n warden get pods,pvc 2>/dev/null
  kubectl -n warden-sandboxes get pods 2>/dev/null || true
  kubectl -n warden get certificate,ingress 2>/dev/null || true
  echo "ingress address: $(ingress_ip)"
  cmd_dns 2>/dev/null | grep -E '^(delegation|\*?\.?'"$DOMAIN"')' || true
}

cmd_park() {
  load_env
  # Disks stay (pd-balanced, $0.10 per GiB-month) and so does the ingress
  # controller's load balancer (about $18 a month); `down` removes both.
  kubectl -n warden scale deploy --all --replicas=0
  kubectl -n warden-sandboxes delete pods --all --wait=false 2>/dev/null || true
}

cmd_unpark() {
  load_env
  kubectl -n warden scale deploy --all --replicas=1
}

cmd_down() {
  load_env
  # Deleting the cluster deletes its persistent disks: the state PVCs and
  # every workspace. The zone and its delegation, the images and the
  # service account stay for the next `up`.
  gcloud_ container clusters delete "$CLUSTER" --region "$REGION" || true
  rm -f "$KUBECONFIG"
}

cmd_delete() {
  load_env
  cmd_down
  gcloud_ dns record-sets delete "${DOMAIN}." --type A --zone "$ZONE" >/dev/null 2>&1 || true
  gcloud_ dns record-sets delete "*.${DOMAIN}." --type A --zone "$ZONE" >/dev/null 2>&1 || true
  gcloud_ dns managed-zones delete "$ZONE" || true
  gcloud_ artifacts repositories delete warden --location "$REGION" || true
  gcloud_ projects remove-iam-policy-binding "$PROJECT" --member "serviceAccount:${GSA}" --role roles/dns.admin --condition=None >/dev/null 2>&1 || true
  gcloud_ iam service-accounts delete "$GSA" || true
  rm -rf "$ROOT/dist/gke"
  echo "remove the NS records that delegated ${DOMAIN} at the parent domain's DNS"
}

case "${1:-}" in
  up) shift; cmd_up "$@" ;;
  kubeconfig) load_env; echo "$KUBECONFIG" ;;
  dns) shift; cmd_dns "$@" ;;
  build-images) shift; cmd_build_images "$@" ;;
  secrets) shift; cmd_secrets "$@" ;;
  deploy) shift; cmd_deploy "$@" ;;
  status) shift; cmd_status "$@" ;;
  park) cmd_park ;;
  unpark) cmd_unpark ;;
  down) cmd_down ;;
  delete) cmd_delete ;;
  *) sed -n '2,18p' "$0"; exit 2 ;;
esac
