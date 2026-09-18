{{/*
Names and labels. Namespaced objects have fixed names (warden-<component>)
because the service identities in the certificates and the addresses in
warden.json are those names (appendix A); one release per namespace.
Cluster-scoped objects are prefixed with the sandbox namespace name, which
is unique per installation.
*/}}

{{- define "warden.chart" -}}
{{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "warden.sandboxNamespace" -}}
{{ .Values.sandboxNamespace.name }}
{{- end -}}

{{- define "warden.clusterPrefix" -}}
{{ printf "warden-%s" .Values.sandboxNamespace.name | trunc 50 | trimSuffix "-" }}
{{- end -}}

{{/* Common labels for every object. */}}
{{- define "warden.labels" -}}
app.kubernetes.io/name: warden
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ include "warden.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ include "warden.chart" . }}
{{- end -}}

{{/* Selector labels for one component (policy, runner, chat, edge). */}}
{{- define "warden.selectorLabels" -}}
app.kubernetes.io/name: warden
app.kubernetes.io/instance: {{ .root.Release.Name }}
warden.monaddle.com/component: {{ .component }}
{{- end -}}

{{/* The image reference: repository@digest when pinned, else repository:tag. */}}
{{- define "warden.imageTag" -}}
{{ default .Chart.AppVersion .Values.image.tag }}
{{- end -}}

{{- define "warden.image" -}}
{{- if .Values.image.digest -}}
{{ printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else -}}
{{ printf "%s:%s" .Values.image.repository (include "warden.imageTag" .) }}
{{- end -}}
{{- end -}}

{{/* The RuntimeClass sandbox pods must use. */}}
{{- define "warden.runtimeClassName" -}}
{{- if .Values.runtime.runtimeClassName -}}
{{ .Values.runtime.runtimeClassName }}
{{- else if eq .Values.runtime.tier "kata" -}}
kata-qemu
{{- else if eq .Values.runtime.tier "gvisor" -}}
gvisor
{{- else -}}
{{ fail (printf "runtime.tier must be \"kata\" or \"gvisor\", not %q" .Values.runtime.tier) }}
{{- end -}}
{{- end -}}

{{/* The handler of the selected tier, for the optional RuntimeClass. */}}
{{- define "warden.runtimeHandler" -}}
{{ index .Values.runtime.handlers .Values.runtime.tier }}
{{- end -}}

{{/* The URL browsers open. */}}
{{- define "warden.publicURL" -}}
{{- if .Values.auth.publicURL -}}
{{ .Values.auth.publicURL }}
{{- else if eq .Values.auth.mode "owner" -}}
{{ printf "http://127.0.0.1:%d" (int .Values.edge.port) }}
{{- else -}}
{{ fail "auth.publicURL is required in google mode" }}
{{- end -}}
{{- end -}}

{{/* The app host of the Ingress: previews.ingress.host or the host of auth.publicURL. */}}
{{- define "warden.publicHost" -}}
{{- if .Values.previews.ingress.host -}}
{{ .Values.previews.ingress.host }}
{{- else -}}
{{ include "warden.publicURL" . | trimPrefix "https://" | trimPrefix "http://" | splitList "/" | first | splitList ":" | first }}
{{- end -}}
{{- end -}}

{{/* A NetworkPolicyPeer list item selecting one core-namespace component. */}}
{{- define "warden.peer" -}}
- namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: {{ .root.Release.Namespace }}
  podSelector:
    matchLabels:
      {{- include "warden.selectorLabels" (dict "root" .root "component" .component) | nindent 6 }}
{{- end -}}

{{/* Number of sandbox pods the quota allows. */}}
{{- define "warden.sandboxPods" -}}
{{ add (int .Values.sandboxes.maxRunning) (int .Values.sandboxes.warmSpares) (int .Values.sandboxes.extraPods) }}
{{- end -}}

{{/* Sanity checks shared by every template. */}}
{{- define "warden.validate" -}}
{{- $_ := include "warden.runtimeClassName" . -}}
{{- if not (has .Values.previews.mode (list "loopback" "public")) -}}
{{ fail (printf "previews.mode must be \"loopback\" or \"public\", not %q" .Values.previews.mode) }}
{{- end -}}
{{- if not (has .Values.auth.mode (list "owner" "google")) -}}
{{ fail (printf "auth.mode must be \"owner\" or \"google\", not %q" .Values.auth.mode) }}
{{- end -}}
{{- if and (eq .Values.previews.mode "public") (ne .Values.auth.mode "google") -}}
{{ fail "previews.mode \"public\" requires auth.mode \"google\"" }}
{{- end -}}
{{- if and (eq .Values.previews.mode "public") (not (contains "." .Values.previews.hostSuffix)) -}}
{{ fail "previews.hostSuffix must be a dotted hostname in public mode" }}
{{- end -}}
{{- if and (eq .Values.auth.mode "google") (or (not .Values.auth.google.signInClientID) (not .Values.auth.google.owners)) -}}
{{ fail "auth.mode \"google\" requires auth.google.signInClientID and auth.google.owners" }}
{{- end -}}
{{- if and .Values.tls.bootstrap .Values.tls.certManager.enabled -}}
{{ fail "tls.bootstrap and tls.certManager.enabled are exclusive" }}
{{- end -}}
{{- if and (eq .Values.auth.mode "google") (not (hasPrefix "https://" (include "warden.publicURL" .))) -}}
{{ fail "auth.publicURL must be an https:// URL in google mode" }}
{{- end -}}
{{- if and (eq .Values.auth.mode "owner") (not (hasPrefix "http://" (include "warden.publicURL" .))) -}}
{{ fail "auth.publicURL must be an http://127.0.0.1:<edge.port> URL in owner mode" }}
{{- end -}}
{{- if not (has .Values.egress (list "restricted" "open")) -}}
{{ fail (printf "egress must be \"restricted\" or \"open\", not %q" .Values.egress) }}
{{- end -}}
{{- if not (regexMatch "^sha256:[0-9a-f]{64}$" .Values.guestImage.digest) -}}
{{ fail "guestImage.digest is required: the sha256:<64 hex> platform manifest digest of the guest base image (deploy/guest/build-base.sh prints it)" }}
{{- end -}}
{{- if and .Values.image.digest (not (regexMatch "^sha256:[0-9a-f]{64}$" .Values.image.digest)) -}}
{{ fail "image.digest must be sha256:<64 hex>" }}
{{- end -}}
{{- end -}}

{{/*
The rendered warden.json (appendix A plus the step-1 services/tls fields).
Paths are the container paths the Deployments mount, the same as the
Compose file uses.
*/}}
{{- define "warden.config" -}}
{{- $_ := include "warden.validate" . -}}
{{- $edgeListen := printf "0.0.0.0:%d" (int .Values.edge.port) -}}
{{- $cfg := dict "version" 1 -}}
{{- $_ = set $cfg "runtime" (dict "kind" "kubernetes") -}}
{{- $_ = set $cfg "paths" (dict
      "state" "/var/lib/warden"
      "webAssets" "/app/web"
      "githubCatalog" "/app/vendor"
      "sandboxPolicyTemplate" "/app/config/policy.template.json") -}}
{{- $_ = set $cfg "services" (dict
      "policy" (dict "listen" (printf "tls://0.0.0.0:%d" (int .Values.services.policy.port)) "address" (printf "tls://warden-policy:%d" (int .Values.services.policy.port)))
      "runner" (dict "listen" (printf "tls://0.0.0.0:%d" (int .Values.services.runner.port)) "address" (printf "tls://warden-runner:%d" (int .Values.services.runner.port))
                     "previews" (dict "listen" (printf "tls://0.0.0.0:%d" (int .Values.services.runner.previewPort)) "address" (printf "tls://warden-runner:%d" (int .Values.services.runner.previewPort))))
      "chat"   (dict "listen" (printf "tls://0.0.0.0:%d" (int .Values.services.chat.port)) "address" (printf "tls://warden-chat:%d" (int .Values.services.chat.port)))) -}}
{{- $_ = set $cfg "tls" (dict
      "caFile" "/etc/warden/tls/ca.crt"
      "certFile" "/etc/warden/tls/tls.crt"
      "keyFile" "/etc/warden/tls/tls.key") -}}
{{- $kube := dict
      "namespace" (include "warden.sandboxNamespace" .)
      "tier" .Values.runtime.tier
      "runtimeClass" (include "warden.runtimeClassName" .)
      "guestImage" .Values.guestImage.repository
      "guestImageDigest" .Values.guestImage.digest
      "storageClass" .Values.storage.className
      "workspaceSizeGi" (int .Values.storage.workspaceGi)
      "gatewayService" "warden-gateway"
      "gatewayPort" (int .Values.gateway.port)
      "trustConfigMap" "warden-guest-trust" -}}
{{- if .Values.sandboxes.nodeSelector -}}
{{- $_ = set $kube "nodeSelector" .Values.sandboxes.nodeSelector -}}
{{- end -}}
{{- if .Values.sandboxes.tolerations -}}
{{- $_ = set $kube "tolerations" .Values.sandboxes.tolerations -}}
{{- end -}}
{{- $_ = set $cfg "kubernetes" $kube -}}
{{- $_ = set $cfg "sandboxes" (dict
      "memoryMB" (int .Values.sandboxes.memoryMB)
      "cpus" .Values.sandboxes.cpus
      "maxMemoryMB" (int .Values.sandboxes.maxMemoryMB)
      "maxCPUs" .Values.sandboxes.maxCPUs
      "maxRunning" (int .Values.sandboxes.maxRunning)
      "warmSpares" (int .Values.sandboxes.warmSpares)
      "stopAfterIdleMinutes" (int .Values.sandboxes.stopAfterIdleMinutes)
      "keepStopped" (int .Values.sandboxes.keepStopped)
      "egress" .Values.egress) -}}
{{- $_ = set $cfg "chat" (dict "listen" "127.0.0.1:18780") -}}
{{- if eq .Values.previews.mode "public" -}}
{{- $_ = set $cfg "previews" (dict "mode" "public" "hostSuffix" .Values.previews.hostSuffix "edgeListen" $edgeListen) -}}
{{- else -}}
{{- $_ = set $cfg "previews" (dict "mode" "loopback" "hostSuffix" "localhost" "edgeListen" $edgeListen) -}}
{{- end -}}
{{- $auth := dict "mode" .Values.auth.mode "publicURL" (include "warden.publicURL" .) -}}
{{- if eq .Values.auth.mode "google" -}}
{{- $google := dict
      "signInClientID" .Values.auth.google.signInClientID
      "owners" .Values.auth.google.owners
      "signInLedger" "/var/lib/warden/edge/logins.json" -}}
{{- if .Values.auth.google.demoDomains -}}
{{- $_ = set $google "demoDomains" .Values.auth.google.demoDomains -}}
{{- end -}}
{{- $_ = set $auth "google" $google -}}
{{- end -}}
{{- $_ = set $cfg "auth" $auth -}}
{{- $providers := dict -}}
{{- if .Values.providers.codex.enabled -}}
{{- $_ = set $providers "codex" (dict "secret" .Values.secrets.codex) -}}
{{- else -}}
{{- $_ = set $providers "codex" nil -}}
{{- end -}}
{{- if .Values.providers.claude.enabled -}}
{{- $claude := dict "secret" .Values.secrets.claude -}}
{{- if .Values.providers.claude.allowFastMode -}}
{{- $_ = set $claude "allowFastMode" true -}}
{{- end -}}
{{- if .Values.providers.claude.allowLongContext -}}
{{- $_ = set $claude "allowLongContext" true -}}
{{- end -}}
{{- $_ = set $providers "claude" $claude -}}
{{- else -}}
{{- $_ = set $providers "claude" nil -}}
{{- end -}}
{{- if .Values.providers.google.enabled -}}
{{- $_ = set $providers "google" (dict "docsClient" .Values.providers.google.docsClient) -}}
{{- else -}}
{{- $_ = set $providers "google" nil -}}
{{- end -}}
{{- if .Values.providers.github.enabled -}}
{{- $gh := dict "secret" .Values.secrets.github -}}
{{- if .Values.providers.github.appID -}}
{{- $_ = set $gh "appID" (int64 .Values.providers.github.appID) -}}
{{- $_ = set $gh "appSlug" .Values.providers.github.appSlug -}}
{{- $_ = set $gh "installationOwner" .Values.providers.github.installationOwner -}}
{{- end -}}
{{- $_ = set $providers "github" $gh -}}
{{- else -}}
{{- $_ = set $providers "github" nil -}}
{{- end -}}
{{- $_ = set $cfg "providers" $providers -}}
{{ $cfg | toPrettyJson }}
{{- end -}}

{{/* The provider Secret names the policy Role grants, deduplicated. */}}
{{- define "warden.providerSecretNames" -}}
{{- $names := list -}}
{{- if .Values.providers.codex.enabled }}{{ $names = append $names .Values.secrets.codex }}{{ end -}}
{{- if .Values.providers.claude.enabled }}{{ $names = append $names .Values.secrets.claude }}{{ end -}}
{{- if .Values.providers.github.enabled }}{{ $names = append $names .Values.secrets.github }}{{ end -}}
{{ $names | uniq | toJson }}
{{- end -}}

{{/* Identities and their TLS Secret names. */}}
{{- define "warden.identities" -}}
{{ list "warden-policy" "warden-runner" "warden-chat" "warden-edge" | toJson }}
{{- end -}}

{{/*
The four workloads (plan decision 5): component, warden subcommand, the
state directory under /var/lib/warden (its PVC is warden-<state>-state),
whether the pod needs an API token, the stop grace period the Compose
file gives it, and its container ports.
*/}}
{{- define "warden.components" -}}
{{- $v := .Values -}}
{{- list
  (dict "name" "policy" "subcommand" "policy" "state" "policy" "token" true "grace" 30
        "ports" (list (dict "name" "gateway" "port" (int $v.gateway.port)) (dict "name" "control" "port" (int $v.services.policy.port))))
  (dict "name" "runner" "subcommand" "runner" "state" "runner" "token" true "grace" 45
        "ports" (list (dict "name" "control" "port" (int $v.services.runner.port)) (dict "name" "previews" "port" (int $v.services.runner.previewPort))))
  (dict "name" "chat" "subcommand" "serve" "state" "app" "token" false "grace" 30
        "ports" (list (dict "name" "control" "port" (int $v.services.chat.port))))
  (dict "name" "edge" "subcommand" "edge" "state" "edge" "token" false "grace" 30
        "ports" (list (dict "name" "http" "port" (int $v.edge.port))))
  | toJson }}
{{- end -}}
