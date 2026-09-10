{{/*
Names, labels and the two or three expressions that would otherwise be repeated
in fifteen files.

THE SERVICE NAMES ARE NOT PREFIXED, and that is a decision rather than an
oversight. `deploy/web/nginx.conf` proxies to `controller:8080` and
`deploy/zitadel/nginx.conf` proxies to `zitadel:8080` and
`zitadel-login:3000`. Those files are baked into images and mounted by
docker-compose.yml, and they are the same files here - which is the whole point
of the split in docs/design/27. A release-prefixed Service would mean a second
copy of both, differing in three words, and a class of bug where the compose
stack works and the cluster 502s.

The consequence, stated plainly: ONE RELEASE PER NAMESPACE. That is already the
deployment model - lab in one namespace, production in another, possibly in
different clusters - so it costs nothing. Two releases in one namespace collide
on Service names and Helm says so at install time.
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

{{/* Labels every object carries. app.kubernetes.io/version is the IMAGE
     version, so `kubectl get deploy -L app.kubernetes.io/version` answers what
     is actually running rather than which chart delivered it. */}}
{{- define "swgw.labels" -}}
helm.sh/chart: {{ include "swgw.chart" . }}
app.kubernetes.io/name: {{ include "swgw.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ include "swgw.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: software-gateway
{{- end -}}

{{/* Selector labels for one component. Call as (dict "ctx" $ "component" "worker").
     `app` is included because deploy/web/nginx.conf and the NetworkPolicies
     select on it, and because it is what the compose stack calls the same
     process. */}}
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

{{/* THE IMAGE TAG, in one place. Empty `image.tag` means the chart's
     appVersion, which the pipeline stamps to the version of the last commit
     that touched code. All three images share it: they are built from one
     commit and are one artifact. */}}
{{- define "swgw.imageTag" -}}
{{- default .Chart.AppVersion .Values.image.tag -}}
{{- end -}}

{{/* Call as (dict "ctx" $ "name" "coordinator"). */}}
{{- define "swgw.image" -}}
{{- $i := .ctx.Values.image -}}
{{- $tag := include "swgw.imageTag" .ctx -}}
{{- if $i.registry -}}
{{- printf "%s/%s/software-gateway-%s:%s" $i.registry $i.repository .name $tag -}}
{{- else -}}
{{- printf "%s/software-gateway-%s:%s" $i.repository .name $tag -}}
{{- end -}}
{{- end -}}

{{- define "swgw.imagePullSecrets" -}}
{{- $secrets := .Values.image.pullSecrets -}}
{{- if and .Values.secrets.registryPullSecret.enabled (ne .Values.secrets.backend "none") -}}
{{- $secrets = append $secrets (dict "name" .Values.secrets.registryPullSecret.name) -}}
{{- end -}}
{{- if $secrets }}
imagePullSecrets:
{{- range $secrets }}
  - name: {{ .name }}
{{- end }}
{{- end -}}
{{- end -}}

{{/* --------------------------------------------------------------- security
     Identical for every workload this repository builds, because they have
     identical needs: nothing writes to disk, nothing needs a capability, and
     nothing runs as root. Stated once so a new component cannot quietly get
     a weaker one. */}}
{{- define "swgw.podSecurityContext" -}}
runAsNonRoot: true
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "swgw.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: ["ALL"]
{{- end -}}

{{/* --------------------------------------------------------------- rollout
     maxUnavailable 0: a replica is removed only after its replacement is
     READY. On the Coordinator, ready means the database answers, the schema is
     the one this build expects and products loaded - so a bad image stalls the
     rollout with the old pods still serving instead of taking capacity away. */}}
{{- define "swgw.strategy" -}}
type: RollingUpdate
rollingUpdate:
  maxSurge: {{ .Values.rollout.maxSurge }}
  maxUnavailable: {{ .Values.rollout.maxUnavailable }}
{{- end -}}

{{/* -------------------------------------------------------------- the DSN
     One Secret, whichever database is in use, so nothing downstream has to
     know which. */}}
{{- define "swgw.dbSecretName" -}}
{{- if .Values.postgresql.external.enabled -}}
{{- required "postgresql.external.existingSecret is required when postgresql.external.enabled" .Values.postgresql.external.existingSecret -}}
{{- else -}}
{{- printf "%s-postgres" (include "swgw.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* ------------------------------------------------- configuration mounts
     The three paths every component that reads product configuration uses.
     They are the SAME paths docker-compose.yml mounts, which is why
     config/config.yaml needs no cluster-specific fork: configDir moves, and
     the loader derives the rest.

     products/ and secrets/ are NOT subPath mounts. A subPath mount does not
     receive ConfigMap or Secret updates, which would silently break both the
     product watcher and credential rotation - the two things this deployment
     relies on to change without a restart. config.yaml IS a subPath mount,
     deliberately: a change to it genuinely requires a restart, and its
     checksum annotation is what performs one. */}}
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
    # EVERY inventory entry, and `optional` on each one. A missing credential
    # must take ONE PRODUCT out of service and say which file it looked for -
    # which is what internal/product/secrets.go does - rather than leaving the
    # pod unschedulable and the whole deployment down.
    sources:
{{- range (include "swgw.secretNames" . | fromYamlArray) }}
      - secret:
          name: {{ . }}
          optional: true
{{- end }}
{{- if not (include "swgw.secretNames" . | fromYamlArray) }}
      # No entries in config/secrets/secrets.yaml. An empty projected volume is
      # still mounted, so the path exists and an anonymous estate works.
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

{{/* The names of the Secrets the inventory produces, as a YAML list so callers
     can `fromYamlArray` it. Read from the STAGED config directory, so the list
     is whatever config/secrets/secrets.yaml says and nothing has to be
     restated in values. */}}
{{- define "swgw.secretNames" -}}
{{- $inv := .Files.Get "files/config/secrets/secrets.yaml" | fromYaml -}}
{{- range (default (list) $inv.secrets) }}
- {{ .name }}
{{- end }}
{{- end -}}

{{/* ------------------------------------------------------- shared env
     What both the Coordinator and the Worker need. The two SWGW_ overrides
     are the ONLY difference between this file in a container and the same file
     on a laptop: configDir moves, and secretsDir is derived from it. */}}
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

{{/* ZITADEL's external URL, taken apart the way ZITADEL wants it. It refuses a
     Host header it does not know and stamps this into every token's `iss`, so
     these three are derived from one value rather than set three times. */}}
{{- define "swgw.identityUrl" -}}
{{- .Values.externalUrls.identity | trimSuffix "/" -}}
{{- end -}}

{{- define "swgw.identityHostHeader" -}}
{{- $u := urlParse (include "swgw.identityUrl" .) -}}
{{- $u.host -}}
{{- end -}}

{{- define "swgw.identityDomain" -}}
{{- $h := include "swgw.identityHostHeader" . -}}
{{- (splitList ":" $h) | first -}}
{{- end -}}

{{- define "swgw.identityPort" -}}
{{- $u := urlParse (include "swgw.identityUrl" .) -}}
{{- $parts := splitList ":" $u.host -}}
{{- if gt (len $parts) 1 -}}
{{- index $parts 1 -}}
{{- else if eq $u.scheme "https" -}}
443
{{- else -}}
80
{{- end -}}
{{- end -}}

{{- define "swgw.identitySecure" -}}
{{- $u := urlParse (include "swgw.identityUrl" .) -}}
{{- if eq $u.scheme "https" }}true{{ else }}false{{ end -}}
{{- end -}}

{{- define "swgw.webUrl" -}}
{{- .Values.externalUrls.web | trimSuffix "/" -}}
{{- end -}}

{{/* THE HASH THAT DECIDES WHETHER PEOPLE GET PROVISIONED.
     Only the two files the seeder reads for identity, plus the settings that
     change what it writes. A product added to config/products does not appear
     here, because adding a product does not change who may sign in - it is the
     seeder's OTHER input, and the ownership check reads it on the run that
     does happen. */}}
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

{{/* --------------------------------------------------- the two nginx tiers
     WEAKER THAN EVERY OTHER WORKLOAD HERE, and it is worth saying why rather
     than letting a reader assume it was forgotten.

     nginx:alpine runs its master as root and binds port 80, and both entrypoints
     WRITE at container start: the web tier renders /runtime-config.json into
     the document root (the SPA's issuer and OIDC client id are not knowable at
     build time), and both fill in nginx's `resolver` with the engine's embedded
     DNS address. So neither runAsNonRoot nor readOnlyRootFilesystem can hold
     without changing the images, and an image change would fork the two tiers
     from the ones docker-compose.yml runs.

     What IS held: no privilege escalation, every capability dropped except the
     one that binding 80 requires, and the RuntimeDefault seccomp profile.
     These containers hold no credential and terminate no TLS. */}}
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

{{/* ------------------------------------------------------ ZITADEL's settings
     The core and the setup Job take the SAME environment: setup writes the
     first instance from it, start reads the rest of it. Stating it twice is
     how the two drift, and a drift here is a login flow that half-works.

     The four LOGINV2 values tell the core where the sign-in screens are
     reachable FROM A BROWSER, which is the front door's address and never the
     Service name. v4 defaults to /ui/v2/login already; they are set anyway
     because "it happened to default that way" is not a thing to build a login
     flow on. */}}
{{- define "swgw.zitadelEnv" -}}
{{- $db := include "swgw.dbSecretName" . -}}
{{- $full := include "swgw.fullname" . -}}
{{- $url := include "swgw.identityUrl" . -}}
- name: ZITADEL_MASTERKEY
  valueFrom:
    secretKeyRef:
      name: {{ if .Values.identity.masterkey.existingSecret }}{{ .Values.identity.masterkey.existingSecret }}{{ else }}{{ $full }}-identity{{ end }}
      key: {{ if .Values.identity.masterkey.existingSecret }}{{ .Values.identity.masterkey.key }}{{ else }}masterkey{{ end }}
- {name: ZITADEL_DATABASE_POSTGRES_HOST, valueFrom: {secretKeyRef: {name: {{ $db }}, key: host}}}
- {name: ZITADEL_DATABASE_POSTGRES_PORT, valueFrom: {secretKeyRef: {name: {{ $db }}, key: port}}}
- {name: ZITADEL_DATABASE_POSTGRES_USER_USERNAME, valueFrom: {secretKeyRef: {name: {{ $db }}, key: user}}}
- {name: ZITADEL_DATABASE_POSTGRES_USER_PASSWORD, valueFrom: {secretKeyRef: {name: {{ $db }}, key: password}}}
- {name: ZITADEL_DATABASE_POSTGRES_USER_SSL_MODE, valueFrom: {secretKeyRef: {name: {{ $db }}, key: sslmode}}}
- {name: ZITADEL_DATABASE_POSTGRES_ADMIN_USERNAME, valueFrom: {secretKeyRef: {name: {{ $db }}, key: user}}}
- {name: ZITADEL_DATABASE_POSTGRES_ADMIN_PASSWORD, valueFrom: {secretKeyRef: {name: {{ $db }}, key: password}}}
- {name: ZITADEL_DATABASE_POSTGRES_ADMIN_SSL_MODE, valueFrom: {secretKeyRef: {name: {{ $db }}, key: sslmode}}}
# ZITADEL gets its OWN database in the same instance, never a shared one: it
# claims `public` and about 150 tables.
- {name: ZITADEL_DATABASE_POSTGRES_DATABASE, value: zitadel}
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
