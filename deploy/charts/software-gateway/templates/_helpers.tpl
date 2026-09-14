{{/*
Names, labels, images, security contexts and the expressions that would
otherwise be repeated in fifteen files.

THE SERVICE NAMES ARE NOT RELEASE-PREFIXED. deploy/web/nginx.conf proxies to
`controller:8080` and deploy/zitadel/nginx.conf proxies to `zitadel:8080` and
`zitadel-login:3000`; those files are baked into images and mounted by
docker-compose.yml, and they are the same files here. The consequence is one
release per namespace, which is already the deployment model.
*/}}

{{- define "swgw.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "swgw.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "swgw.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* app.kubernetes.io/version is the IMAGE version, so `kubectl get deploy -L
     app.kubernetes.io/version` answers what is running. */}}
{{- define "swgw.labels" -}}
helm.sh/chart: {{ include "swgw.chart" . }}
app.kubernetes.io/name: {{ include "swgw.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ include "swgw.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: software-gateway
{{- end -}}

{{/* (dict "ctx" $ "component" "worker"). `app` is included because the two
     nginx configs and the NetworkPolicies select on it. */}}
{{- define "swgw.selectorLabels" -}}
app.kubernetes.io/name: {{ include "swgw.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
app: {{ .component }}
{{- end -}}

{{- define "swgw.componentLabels" -}}
{{ include "swgw.labels" .ctx }}
app.kubernetes.io/component: {{ .component }}
app: {{ .component }}
{{- end -}}

{{/* ------------------------------------------------------------------ images */}}

{{- define "swgw.imageTag" -}}
{{- default .Chart.AppVersion .Values.images.tag -}}
{{- end -}}

{{/* (dict "ctx" $ "name" "coordinator") */}}
{{- define "swgw.image" -}}
{{- $i := .ctx.Values.images -}}
{{- $tag := include "swgw.imageTag" .ctx -}}
{{- $path := printf "%s/software-gateway-%s" $i.repository .name -}}
{{- if $i.registry -}}
{{- printf "%s/%s:%s" $i.registry $path $tag -}}
{{- else -}}
{{- printf "%s:%s" $path $tag -}}
{{- end -}}
{{- end -}}

{{/* A third-party image, resolved through the mirror.

     Every one is written as its FULL upstream reference, so an empty mirror
     pulls it from where it actually lives - ZITADEL and CloudNativePG publish to
     ghcr.io and nowhere else, and a host-less path would send a public
     evaluation to a Docker Hub repository that does not exist.

     The mirror REPLACES that host rather than being prefixed to it, so
     `ghcr.io/zitadel/zitadel:v4.17.3` under mirror `reg.internal/docker` becomes
     `reg.internal/docker/zitadel/zitadel:v4.17.3`. Prefixing would produce
     `reg.internal/docker/ghcr.io/zitadel/...`, which no aggregating repository
     serves.

     (dict "ctx" $ "image" .ctx.Values.images.nginx) */}}
{{- define "swgw.mirroredImage" -}}
{{- $mirror := .ctx.Values.images.mirror | trimSuffix "/" -}}
{{- if $mirror -}}
{{- $parts := splitList "/" .image -}}
{{- $first := index $parts 0 -}}
{{- if and (gt (len $parts) 1) (or (contains "." $first) (contains ":" $first)) -}}
{{- $parts = rest $parts -}}
{{- end -}}
{{- printf "%s/%s" $mirror (join "/" $parts) -}}
{{- else -}}
{{- .image -}}
{{- end -}}
{{- end -}}

{{/* Every pull Secret this release uses, as a YAML list of names. The one the
     credentials backend renders is included without being restated. */}}
{{- define "swgw.pullSecretNames" -}}
{{- $names := default (list) .Values.images.pullSecrets -}}
{{- $pull := .Values.secrets.registryPullSecret -}}
{{- if and $pull.enabled (ne .Values.secrets.backend "none") -}}
{{- $names = append $names $pull.name -}}
{{- end -}}
{{- range ($names | uniq) }}
- {{ . }}
{{- end }}
{{- end -}}

{{- define "swgw.imagePullSecrets" -}}
{{- $names := include "swgw.pullSecretNames" . | fromYamlArray -}}
{{- if $names }}
imagePullSecrets:
{{- range $names }}
  - name: {{ . }}
{{- end }}
{{- end -}}
{{- end -}}

{{/* ---------------------------------------------------------------- security */}}

{{- define "swgw.podSecurityContext" -}}
runAsNonRoot: true
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "swgw.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
runAsUser: 1000
runAsGroup: 1000
capabilities:
  drop: ["ALL"]
{{- end -}}

{{/* WEAKER THAN EVERY OTHER WORKLOAD HERE, and deliberately. nginx:alpine runs
     its master as root and binds port 80, and both entrypoints write at start:
     the web tier renders /runtime-config.json, and both fill in nginx's
     `resolver` from /etc/resolv.conf. Neither runAsNonRoot nor a read-only root
     can hold without forking the images compose runs. These containers hold no
     credential and terminate no TLS. */}}
{{- define "swgw.nginxContainerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: false
capabilities:
  drop: ["ALL"]
  add: ["NET_BIND_SERVICE", "CHOWN", "SETGID", "SETUID"]
{{- end -}}

{{- define "swgw.nginxPodSecurityContext" -}}
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "swgw.strategy" -}}
type: RollingUpdate
rollingUpdate:
  maxSurge: {{ .Values.rollout.maxSurge }}
  maxUnavailable: {{ .Values.rollout.maxUnavailable }}
{{- end -}}

{{/* ---------------------------------------------------------------- database */}}

{{/* CloudNativePG publishes the owner's connection as `<cluster>-app` and its
     read-write endpoint as `<cluster>-rw`, which follows the primary through a
     failover. Deriving both from one name is what keeps a renamed cluster from
     being a three-line change with two places to get wrong. */}}
{{- define "swgw.dbHost" -}}
{{- if .Values.database.host -}}
{{- .Values.database.host -}}
{{- else -}}
{{- printf "%s-rw" (required "database.host is required when database.cluster.name is empty" .Values.database.cluster.name) -}}
{{- end -}}
{{- end -}}

{{- define "swgw.dbSecretName" -}}
{{- if .Values.database.existingSecret -}}
{{- .Values.database.existingSecret -}}
{{- else -}}
{{- printf "%s-app" (required "database.existingSecret is required when database.cluster.name is empty" .Values.database.cluster.name) -}}
{{- end -}}
{{- end -}}

{{/* The DSN, from a key when the Secret carries one and composed by the kubelet
     otherwise.

     `$(VAR)` in an env value is expanded from variables declared EARLIER in the
     same container, so the password reaches the process without being written
     into a manifest and without a shell in a distroless image. The ordering is
     load-bearing: a `$(VAR)` naming a later variable is passed through as
     literal text. */}}
{{- define "swgw.dbEnv" -}}
{{- $db := include "swgw.dbSecretName" . -}}
{{- if .Values.database.dsnKey }}
- name: SWGW_DATABASE_DSN
  valueFrom:
    secretKeyRef:
      name: {{ $db }}
      key: {{ .Values.database.dsnKey }}
{{- else }}
- name: SWGW_DB_USER
  valueFrom:
    secretKeyRef: {name: {{ $db }}, key: {{ .Values.database.usernameKey }}}
- name: SWGW_DB_PASSWORD
  valueFrom:
    secretKeyRef: {name: {{ $db }}, key: {{ .Values.database.passwordKey }}}
- name: SWGW_DATABASE_DSN
  value: postgres://$(SWGW_DB_USER):$(SWGW_DB_PASSWORD)@{{ include "swgw.dbHost" . }}:{{ .Values.database.port }}/{{ .Values.database.name }}?sslmode={{ .Values.database.sslMode }}
{{- end }}
{{- end -}}

{{/* ----------------------------------------------------- configuration mounts
     The same paths docker-compose.yml mounts, which is why config/config.yaml
     needs no cluster-specific fork.

     products/ and secrets/ are NOT subPath mounts: a subPath does not receive
     ConfigMap or Secret updates, and the product watcher and credential
     rotation are the two things this deployment relies on to change without a
     restart. config.yaml IS a subPath mount, because a change to it genuinely
     requires one and its checksum annotation performs it. */}}
{{- define "swgw.configVolumeMounts" -}}
- name: system-config
  mountPath: /etc/softwaregateway/config.yaml
  subPath: config.yaml
  readOnly: true
- name: products
  mountPath: /etc/softwaregateway/products
  readOnly: true
- name: secrets
  mountPath: /etc/softwaregateway/secrets
  readOnly: true
{{- if .Values.extraCAs.enabled }}
- name: extra-cas
  mountPath: /etc/ssl/certs-extra
  readOnly: true
{{- end }}
{{- end -}}

{{- define "swgw.configVolumes" -}}
- name: system-config
  configMap:
    name: {{ include "swgw.fullname" . }}-system
- name: products
  configMap:
    name: {{ include "swgw.fullname" . }}-products
- name: secrets
  projected:
    # `optional` on each entry: a missing credential must take ONE PRODUCT out
    # of service and say which file it looked for, not leave the pod
    # unschedulable.
    sources:
{{- range (include "swgw.secretNames" . | fromYamlArray) }}
      - secret:
          name: {{ . }}
          optional: true
{{- end }}
{{- if not (include "swgw.secretNames" . | fromYamlArray) }}
      - downwardAPI:
          items:
            - path: .keep
              fieldRef: {fieldPath: metadata.name}
{{- end }}
{{- if .Values.extraCAs.enabled }}
- name: extra-cas
  configMap:
    name: {{ if .Values.extraCAs.existingConfigMap }}{{ .Values.extraCAs.existingConfigMap }}{{ else }}{{ include "swgw.fullname" . }}-extra-cas{{ end }}
{{- end }}
{{- end -}}

{{/* The Secrets projected into /etc/softwaregateway/secrets, from the staged
     inventory.

     PULL CREDENTIALS ARE LEFT OUT. They are read by the kubelet, not by any
     process in these containers, so projecting one would put a credential for a
     registry the application never talks to inside the directory it reads
     credentials from. That holds however the Secret came to exist - rendered by
     the backend, or created by hand and named in imagePullSecrets. */}}
{{- define "swgw.secretNames" -}}
{{- $inv := .Files.Get "files/config/secrets/secrets.yaml" | fromYaml -}}
{{- $pull := include "swgw.pullSecretNames" . | fromYamlArray -}}
{{- if .Values.secrets.registryPullSecret.enabled -}}
{{- $pull = append $pull .Values.secrets.registryPullSecret.from -}}
{{- end -}}
{{- range (default (list) $inv.secrets) }}
{{- if not (has .name $pull) }}
- {{ .name }}
{{- end }}
{{- end }}
{{- end -}}

{{/* The only difference between config.yaml in a container and the same file on
     a laptop: configDir moves, and secretsDir is derived from it. */}}
{{- define "swgw.commonEnv" -}}
- name: SWGW_CONFIGDIR
  value: /etc/softwaregateway
- name: SWGW_SECRETSDIR
  value: /etc/softwaregateway/secrets
{{- if .Values.extraCAs.enabled }}
- name: SSL_CERT_FILE
  value: /etc/ssl/certs-extra/ca-certificates.crt
{{- end }}
{{- range $k, $v := .Values.settings }}
- name: {{ $k }}
  value: {{ $v | quote }}
{{- end }}
{{- end -}}

{{/* -------------------------------------------------------------- addresses
     Every browser-facing value is read out of the two URLs in `access`, so the
     issuer a token carries and the host an entry point serves cannot disagree. */}}

{{- define "swgw.webUrl" -}}
{{- required "access.webUrl is required" .Values.access.webUrl | trimSuffix "/" -}}
{{- end -}}

{{- define "swgw.identityUrl" -}}
{{- required "access.identityUrl is required" .Values.access.identityUrl | trimSuffix "/" -}}
{{- end -}}

{{/* (dict "ctx" $ "url" "https://id.example.com:8443") */}}
{{- define "swgw.urlScheme" -}}
{{- (urlParse .).scheme -}}
{{- end -}}

{{/* host[:port], which is what a browser sends and what ZITADEL matches on. */}}
{{- define "swgw.urlAuthority" -}}
{{- (urlParse .).host -}}
{{- end -}}

{{/* The host alone. An Ingress rule and a Route take this, never a port. */}}
{{- define "swgw.urlHost" -}}
{{- (splitList ":" (urlParse .).host) | first -}}
{{- end -}}

{{/* The port a browser really uses, stated even when it is the scheme's
     default - ZITADEL_EXTERNALPORT and a Service port both need a number. */}}
{{- define "swgw.urlPort" -}}
{{- $u := urlParse . -}}
{{- $parts := splitList ":" $u.host -}}
{{- if gt (len $parts) 1 -}}
{{- index $parts 1 -}}
{{- else if eq $u.scheme "https" -}}
443
{{- else -}}
80
{{- end -}}
{{- end -}}

{{- define "swgw.identityHostHeader" -}}
{{- include "swgw.urlAuthority" (include "swgw.identityUrl" .) -}}
{{- end -}}

{{- define "swgw.identityDomain" -}}
{{- include "swgw.urlHost" (include "swgw.identityUrl" .) -}}
{{- end -}}

{{- define "swgw.identityPort" -}}
{{- include "swgw.urlPort" (include "swgw.identityUrl" .) -}}
{{- end -}}

{{- define "swgw.identitySecure" -}}
{{- if eq (include "swgw.urlScheme" (include "swgw.identityUrl" .)) "https" }}true{{ else }}false{{ end -}}
{{- end -}}

{{/* THE HASH THAT DECIDES WHETHER PEOPLE GET PROVISIONED. Only what the seeder
     reads for identity, plus the settings that change what it writes. */}}
{{- define "swgw.identityHash" -}}
{{- $parts := list
      (.Files.Get "files/config/users/users.yaml")
      (.Files.Get "files/config/access/roles.yaml")
      (.Files.Glob "files/config/products/*.yaml" | toYaml)
      (toYaml .Values.identity.sso)
      (toYaml .Values.identity.tokenLifetimes)
      (toYaml .Values.identity.bootstrapAdmin)
      (toString .Values.seed.rotateWorkerSecret)
      (include "swgw.identityUrl" .)
      (include "swgw.webUrl" .)
      .Values.tenant -}}
{{- join "\x00" $parts | sha256sum | trunc 10 -}}
{{- end -}}

{{/* ------------------------------------------------------- ZITADEL's settings
     The core and the setup Job take the SAME environment: setup writes the
     first instance from it, start reads the rest of it. */}}
{{- define "swgw.zitadelEnv" -}}
{{- $db := include "swgw.dbSecretName" . -}}
{{- $full := include "swgw.fullname" . -}}
{{- $url := include "swgw.identityUrl" . -}}
- name: ZITADEL_MASTERKEY
  valueFrom:
    secretKeyRef:
      name: {{ if .Values.identity.masterkey.existingSecret }}{{ .Values.identity.masterkey.existingSecret }}{{ else }}{{ $full }}-identity{{ end }}
      key: {{ if .Values.identity.masterkey.existingSecret }}{{ .Values.identity.masterkey.key }}{{ else }}masterkey{{ end }}
# ZITADEL wants the connection in parts rather than as a URL. The credentials
# are the same two keys the Coordinator reads, so there is one password.
- {name: ZITADEL_DATABASE_POSTGRES_HOST, value: {{ include "swgw.dbHost" . | quote }}}
- {name: ZITADEL_DATABASE_POSTGRES_PORT, value: {{ .Values.database.port | quote }}}
- name: ZITADEL_DATABASE_POSTGRES_USER_USERNAME
  valueFrom: {secretKeyRef: {name: {{ $db }}, key: {{ .Values.database.usernameKey }}}}
- name: ZITADEL_DATABASE_POSTGRES_USER_PASSWORD
  valueFrom: {secretKeyRef: {name: {{ $db }}, key: {{ .Values.database.passwordKey }}}}
- {name: ZITADEL_DATABASE_POSTGRES_USER_SSL_MODE, value: {{ .Values.database.sslMode | quote }}}
# The same account as the application's. It owns the `zitadel` database, so it
# can create every schema ZITADEL needs and nothing outside it - which is what a
# separate superuser would have bought, at the cost of a second credential.
- name: ZITADEL_DATABASE_POSTGRES_ADMIN_USERNAME
  valueFrom: {secretKeyRef: {name: {{ $db }}, key: {{ .Values.database.usernameKey }}}}
- name: ZITADEL_DATABASE_POSTGRES_ADMIN_PASSWORD
  valueFrom: {secretKeyRef: {name: {{ $db }}, key: {{ .Values.database.passwordKey }}}}
- {name: ZITADEL_DATABASE_POSTGRES_ADMIN_SSL_MODE, value: {{ .Values.database.sslMode | quote }}}
- {name: ZITADEL_DATABASE_POSTGRES_DATABASE, value: {{ .Values.database.identityDatabase | quote }}}
- {name: ZITADEL_EXTERNALDOMAIN, value: {{ include "swgw.identityDomain" . | quote }}}
- {name: ZITADEL_EXTERNALPORT, value: {{ include "swgw.identityPort" . | quote }}}
- {name: ZITADEL_EXTERNALSECURE, value: {{ include "swgw.identitySecure" . | quote }}}
- {name: ZITADEL_TLS_ENABLED, value: "false"}
- {name: ZITADEL_DEFAULTINSTANCE_FEATURES_LOGINV2_REQUIRED, value: "true"}
- {name: ZITADEL_DEFAULTINSTANCE_FEATURES_LOGINV2_BASEURI, value: {{ printf "%s/ui/v2/login/" $url | quote }}}
- {name: ZITADEL_OIDC_DEFAULTLOGINURLV2, value: {{ printf "%s/ui/v2/login/login?authRequest=" $url | quote }}}
- {name: ZITADEL_OIDC_DEFAULTLOGOUTURLV2, value: {{ printf "%s/ui/v2/login/logout?post_logout_redirect=" $url | quote }}}
# The org created at first boot IS the tenant. No second step.
- {name: ZITADEL_FIRSTINSTANCE_ORG_NAME, value: {{ .Values.tenant | quote }}}
- {name: ZITADEL_FIRSTINSTANCE_ORG_HUMAN_USERNAME, value: zitadel-admin}
- name: ZITADEL_FIRSTINSTANCE_ORG_HUMAN_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ if .Values.identity.rootPassword.existingSecret }}{{ .Values.identity.rootPassword.existingSecret }}{{ else }}{{ $full }}-identity{{ end }}
      key: {{ if .Values.identity.rootPassword.existingSecret }}{{ .Values.identity.rootPassword.key | default "rootPassword" }}{{ else }}rootPassword{{ end }}
- {name: ZITADEL_FIRSTINSTANCE_ORG_MACHINE_MACHINE_USERNAME, value: seeder}
- {name: ZITADEL_FIRSTINSTANCE_ORG_MACHINE_MACHINE_NAME, value: seeder}
- {name: ZITADEL_FIRSTINSTANCE_ORG_MACHINE_PAT_EXPIRATIONDATE, value: "2100-01-01T00:00:00Z"}
{{- end -}}
