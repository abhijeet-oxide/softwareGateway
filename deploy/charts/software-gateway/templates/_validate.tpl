{{/*
THE CHECKS THAT RUN BEFORE ANYTHING IS APPLIED.

Every one of these is a mistake that produces a stack which comes up GREEN and
does not work, which is the only kind worth failing a render for. A missing
image tag fails loudly on its own and needs nothing here.

They are `fail`, not warnings: a warning in Helm output is read by nobody.
*/}}
{{- define "swgw.validate" -}}

{{- if and (not .Values.layers.application) (not .Values.layers.database) }}
{{- fail "\n\nBoth layers are off, so this release would render nothing.\n\nSet layers.application, layers.database, or both.\n" }}
{{- end }}

{{- if not (has .Values.access.scheme (list "http" "https")) }}
{{- fail (printf "\n\naccess.scheme is %q; it must be http or https.\n" .Values.access.scheme) }}
{{- end }}

{{- $exposeTypes := list "none" "ingress" "route" "loadBalancer" "nodePort" }}
{{- if not (has .Values.access.expose.type $exposeTypes) }}
{{- fail (printf "\n\naccess.expose.type is %q; it must be one of: %s.\n" .Values.access.expose.type (join ", " $exposeTypes)) }}
{{- end }}

{{/* An Ingress or a Route routes on the Host header, so an IP literal there
     matches nothing. A lab without DNS wants loadBalancer, nodePort, or none
     with its own reverse proxy in front. */}}
{{- if has .Values.access.expose.type (list "ingress" "route") }}
{{- range $which := list "web" "identity" }}
{{- $host := (index $.Values.access $which).host }}
{{- if regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$" $host }}
{{- fail (printf "\n\naccess.%s.host is %q and access.expose.type is %q.\n\nAn Ingress and a Route both route on the Host header, which a browser sets to\nan IP only when it was given one - and no controller matches a rule whose host\nis an IP literal.\n\nFor a deployment with no DNS, use access.expose.type loadBalancer or nodePort,\nor none with your own reverse proxy in front of the `web` and `zitadel-proxy`\nServices.\n" $which $host $.Values.access.expose.type) }}
{{- end }}
{{- end }}
{{- end }}

{{- if eq .Values.access.expose.type "nodePort" }}
{{- range $which := list "web" "identity" }}
{{- $port := int (default (index $.Values.access $which).port (index $.Values.access.expose.nodePort $which)) }}
{{- if or (lt $port 30000) (gt $port 32767) }}
{{- fail (printf "\n\nThe %s node port resolves to %d, which is outside the default NodePort range\n30000-32767.\n\nWith access.expose.type nodePort the browser's port IS the node port, so set\naccess.%s.port inside that range - or set access.expose.nodePort.%s to the node\nport and access.%s.port to the port a browser reaches through whatever sits in\nfront of it.\n" $which $port $which $which $which) }}
{{- end }}
{{- end }}
{{- end }}

{{- if and .Values.layers.database (not .Values.database.cluster.name) }}
{{- fail "\n\nlayers.database is on but database.cluster.name is empty.\n\nAn empty name says this deployment does not own its database. Either name the\nCluster to provision, or turn the layer off and set database.host and\ndatabase.existingSecret at the one you already run.\n" }}
{{- end }}

{{- if .Values.layers.application }}

{{- if .Values.identity.enabled }}
{{- $mk := .Values.identity.masterkey.value }}
{{- if and $mk (ne (len $mk) 32) }}
{{- fail (printf "\n\nidentity.masterkey.value is %d bytes and ZITADEL requires exactly 32.\n\nIt refuses to start otherwise, and says so from inside a migration failure - so\nthe symptom names neither this setting nor its length.\n" (len $mk)) }}
{{- end }}

{{/* The seeder makes this same check and refuses the run. Making it here means
     the pull request fails rather than the Job. */}}
{{- if and .Values.identity.sso.issuer .Values.identity.bootstrapAdmin.password }}
{{- fail "\n\nidentity.bootstrapAdmin.password is set while identity.sso.issuer is configured.\n\nThe password shortcut is for local use only. With single sign-on configured the\nadministrator signs in through the directory like everybody else.\n" }}
{{- end }}

{{- if and .Values.identity.sso.issuer (not .Values.identity.sso.existingSecret) }}
{{- fail "\n\nidentity.sso.issuer is set but identity.sso.existingSecret is not.\n\nThe client secret is read from a Secret and never from values: a value here is a\ncredential in Git, in the HelmRelease, and in `helm get values`.\n" }}
{{- end }}
{{- end }}

{{- if and (eq .Values.secrets.backend "none") .Values.secrets.registryPullSecret.enabled }}
{{- fail "\n\nsecrets.registryPullSecret.enabled is set with secrets.backend \"none\".\n\nNothing would create the pull Secret. Either choose a backend, or create the\nSecret yourself and name it in imagePullSecrets.\n" }}
{{- end }}

{{/* HALF-MIRRORED, WHICH IS THE WORST OF THE THREE STATES. `image.registry`
     names where this product's three images come from; `images.mirror` names
     where the five third-party ones come from. Setting the first and not the
     second gives a cluster that pulls some images from the internal registry
     and the rest from the public internet - which is not an air-gapped
     deployment and is indistinguishable from one until the first node without
     egress tries to schedule a pod. */}}
{{- if and .Values.image.registry (not .Values.images.mirror) }}
{{- fail (printf "\n\nimage.registry is %q but images.mirror is empty.\n\nZITADEL, its sign-in screens, nginx, Cerbos and node would be pulled from the\npublic internet while this product's own images came from the internal one.\n\nSet images.mirror to a repository that aggregates the upstreams, e.g.\n\n  images:\n    mirror: %s/docker\n\nor clear image.registry if this really is meant to pull from the internet.\n" .Values.image.registry .Values.image.registry) }}
{{- end }}

{{/* A pull secret enabled without an inventory entry is a deployment whose every
     pod sits in ImagePullBackOff with a Secret that was never going to exist. */}}
{{- if .Values.secrets.registryPullSecret.enabled }}
{{- $inv := .Files.Get "files/config/secrets/secrets.yaml" | fromYaml }}
{{- $names := list }}
{{- range (default (list) $inv.secrets) }}{{ $names = append $names .name }}{{ end }}
{{- if not (has .Values.secrets.registryPullSecret.from $names) }}
{{- fail (printf "\n\nsecrets.registryPullSecret.from is %q, which is not in\nconfig/secrets/secrets.yaml.\n\nEvery pod would be given an imagePullSecret naming a Secret that nothing\ncreates. Declare it in the inventory with the single key .dockerconfigjson.\n\ndeclared: %s\n" .Values.secrets.registryPullSecret.from (join ", " $names)) }}
{{- end }}
{{- end }}

{{/* PRODUCTS THAT REFERENCE A CREDENTIAL NOBODY DECLARED. The Go test makes the
     same check on the pull request; this covers a chart installed from the
     registry with an inventory edited afterwards. */}}
{{- $inv := .Files.Get "files/config/secrets/secrets.yaml" | fromYaml }}
{{- $known := list }}
{{- range (default (list) $inv.secrets) }}{{ $known = append $known .name }}{{ end }}
{{- range $path, $_ := .Files.Glob "files/config/products/*.yaml" }}
{{- $doc := $.Files.Get $path | fromYaml }}
{{- range concat (default (list) (default (dict) $doc.spec).sources) (default (list) (default (dict) $doc.spec).targets) }}
{{- if and .credentialsRef (not (has .credentialsRef.secretName $known)) }}
{{- fail (printf "\n\n%s names credentialsRef.secretName %q, which is not in\nconfig/secrets/secrets.yaml.\n\nNothing would create that Secret, so the product would load, be marked invalid\nfor a missing file, and take itself out of service.\n" (base $path) .credentialsRef.secretName) }}
{{- end }}
{{- end }}
{{- end }}

{{- end }}
{{- end -}}
