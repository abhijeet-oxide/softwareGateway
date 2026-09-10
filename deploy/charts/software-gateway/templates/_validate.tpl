{{/*
THE CHECKS THAT RUN BEFORE ANYTHING IS APPLIED.

Every one of these is a mistake that produces a stack which comes up GREEN and
does not work - which is the only kind worth failing a render for. A missing
image tag fails loudly on its own and needs nothing here.

They are `fail`, not warnings. A warning in Helm output is read by nobody: the
render succeeds, Flux applies it, and the first person to notice is somebody
who cannot sign in.
*/}}
{{- define "swgw.validate" -}}

{{- if not .Values.postgresql.external.enabled }}
{{- if and (not .Values.postgresql.password.value) (not .Values.postgresql.password.existingSecret) }}
{{- fail "\n\npostgresql.password.value is not set.\n\nThe bundled database needs a password, and this chart will not invent one:\na generated password that nothing stores is a database nobody can open after\nthe next `helm upgrade`. Set one of:\n\n  postgresql.password.value          a literal, for a lab namespace\n  postgresql.password.existingSecret a Secret this chart does not manage\n\nOr point at a managed instance with postgresql.external.enabled and\npostgresql.external.existingSecret - which is the recommendation for anything\nthat holds real data.\n" }}
{{- end }}
{{- end }}

{{- if .Values.identity.enabled }}
{{- $mk := .Values.identity.masterkey.value }}
{{- if and $mk (ne (len $mk) 32) }}
{{- fail (printf "\n\nidentity.masterkey.value is %d bytes and ZITADEL requires exactly 32.\n\nIt refuses to start otherwise, and says so from inside a migration failure -\nso the symptom names neither this setting nor its length. Count before\nchanging it.\n" (len $mk)) }}
{{- end }}

{{/* THE MISTAKE THIS PRODUCT HAS ALREADY MADE. externalUrls.identity is
     stamped into every token's `iss` and is what ZITADEL matches the Host
     header against. If it does not name the host the ingress serves, every
     sign-in redirects to an address that answers 404 and the failure reads as
     a ZITADEL fault three services away from this line. */}}
{{- if and .Values.ingress.enabled .Values.ingress.identity.host }}
{{- $want := .Values.ingress.identity.host }}
{{- $have := include "swgw.identityDomain" . }}
{{- if ne $want $have }}
{{- fail (printf "\n\nexternalUrls.identity names %q but ingress.identity.host is %q.\n\nThey must be the same host. ZITADEL stamps externalUrls.identity into every\ntoken's `iss` and answers 404 to a Host header it does not recognise as its\nown, so a mismatch is a deployment where everything is green and nobody can\nsign in.\n" $have $want) }}
{{- end }}
{{- end }}

{{- if and .Values.ingress.enabled .Values.ingress.app.host }}
{{- $u := urlParse (include "swgw.webUrl" .) }}
{{- $have := (splitList ":" $u.host) | first }}
{{- if ne .Values.ingress.app.host $have }}
{{- fail (printf "\n\nexternalUrls.web names %q but ingress.app.host is %q.\n\nThey must be the same host: externalUrls.web is what the seeder registers as\nthe SPA's OIDC redirect URI, and an identity provider refuses a redirect it\nwas not given.\n" $have .Values.ingress.app.host) }}
{{- end }}
{{- end }}

{{/* The seeder makes this same check and refuses the run. Making it here as
     well means the pull request fails rather than the Job. */}}
{{- if and .Values.identity.sso.issuer .Values.identity.bootstrapAdmin.password }}
{{- fail "\n\nidentity.bootstrapAdmin.password is set while identity.sso.issuer is configured.\n\nThe password shortcut is for local use only and the seeder refuses to run with\nboth. Unset the password: with single sign-on configured, the administrator\nsigns in through the directory like everybody else.\n" }}
{{- end }}

{{- if and .Values.identity.sso.issuer (not .Values.identity.sso.existingSecret) }}
{{- fail "\n\nidentity.sso.issuer is set but identity.sso.existingSecret is not.\n\nThe client secret is read from a Secret and never from values: a value here is\na credential in Git, in the HelmRelease, and in `helm get values`. Put it in\nthe backend named by config/secrets/secrets.yaml and name the resulting Secret\nhere.\n" }}
{{- end }}
{{- end }}

{{- if and (eq .Values.secrets.backend "none") .Values.secrets.registryPullSecret.enabled }}
{{- fail "\n\nsecrets.registryPullSecret.enabled is set with secrets.backend \"none\".\n\nNothing would create the pull Secret. Either choose a backend, or create the\nSecret yourself and name it in image.pullSecrets.\n" }}
{{- end }}

{{/* PRODUCTS THAT REFERENCE A CREDENTIAL NOBODY DECLARED. The Go test makes
     the same check on the pull request; this one covers a chart installed from
     the registry with an inventory that was edited afterwards. */}}
{{- $inv := .Files.Get "files/config/secrets/secrets.yaml" | fromYaml }}
{{- $known := list }}
{{- range (default (list) $inv.secrets) }}{{ $known = append $known .name }}{{ end }}
{{- range $path, $_ := .Files.Glob "files/config/products/*.yaml" }}
{{- $doc := $.Files.Get $path | fromYaml }}
{{- range (default (list) (default (dict) $doc.spec).sources) }}
{{- if and .credentialsRef (not (has .credentialsRef.secretName $known)) }}
{{- fail (printf "\n\n%s names credentialsRef.secretName %q, which is not in\nconfig/secrets/secrets.yaml.\n\nNothing would create that Secret, so the product would load, be marked invalid\nfor a missing file, and take itself out of service. Add it to the inventory in\nthe same change as the product.\n" (base $path) .credentialsRef.secretName) }}
{{- end }}
{{- end }}
{{- range (default (list) (default (dict) $doc.spec).targets) }}
{{- if and .credentialsRef (not (has .credentialsRef.secretName $known)) }}
{{- fail (printf "\n\n%s names credentialsRef.secretName %q, which is not in\nconfig/secrets/secrets.yaml.\n\nNothing would create that Secret, so the product would load, be marked invalid\nfor a missing file, and take itself out of service. Add it to the inventory in\nthe same change as the product.\n" (base $path) .credentialsRef.secretName) }}
{{- end }}
{{- end }}
{{- end }}

{{- end -}}
