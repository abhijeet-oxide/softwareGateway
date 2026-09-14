Software Gateway AKS Deployment - Troubleshooting Summary
Environment
Namespace: wst2m0uspa0001c
Cluster: ncd249-nprd-westus2
Chart progression:
0.1.20 -> 0.1.22 -> 0.1.23

Issues Resolved Today
1. Coordinator failed due to unsupported DB environment variables
Error
unknown environment variable(s):
SWGW_DB_USER
SWGW_DB_PASSWORD

Root Cause

Coordinator was configured to consume:

{{ include "swgw.dbCredentialEnv" . }}


which injected:

SWGW_DB_USER
SWGW_DB_PASSWORD


New coordinator image no longer supports those variables.

Fix

Removed:

{{ include "swgw.dbCredentialEnv" . }}


Changed to:

- name: SWGW_DATABASE_DSN
  valueFrom:
    secretKeyRef:
      name: {{ include "swgw.dbSecretName" . }}
      key: uri

Status

✅ Fixed

2. Image pull failures
Error

Pods failing to pull images from:

uspautomation.azurecr.io

Root Cause

Some deployments were missing:

imagePullSecrets:


or were not consuming the chart-level helper correctly.

Fix

Used shared helper:

{{- include "swgw.imagePullSecrets" . | nindent 6 }}


and verified:

uspautomation-acr-secret


is attached.

Status

✅ Fixed

3. Coordinator startup failure due to read-only filesystem
Error
mkdir /tmp/sgw-baseline-xxxx:
read-only file system

Root Cause

Coordinator runs with:

readOnlyRootFilesystem: true


but stages compliance bundles in:

/tmp

Fix

Added:

volumeMounts:
  - name: tmp
    mountPath: /tmp


and

volumes:
  - name: tmp
    emptyDir: {}

Status

✅ Fixed

Evidence:

compliance policies loaded
compliance renderer ready

4. ZITADEL startup failure
Error
'Port' cannot parse value as 'uint16'

Root Cause

Not definitively proven, but disabling Kubernetes service environment injection resolved the issue.

Fix

Added:

spec:
  enableServiceLinks: false


to ZITADEL deployment.

Result

Before:

CrashLoopBackOff


After:

swgw-software-gateway-zitadel
1/1 Running

Status

✅ Fixed

5. Flux not picking up newer chart versions
Symptoms
spec.chart.spec.version = 0.1.22
lastAttemptedRevision = 0.1.20


and later

spec.chart.spec.version = 0.1.23
lastAttemptedRevision = 0.1.22

Cause

Flux waiting on previous failed install cycle.

Fix

Forced reconcile:

flux reconcile helmrelease software-gateway \
-n wst2m0uspa0001c \
--with-source \
--force

Status

✅ Working

Current State

Current pod status:

cloudnative-pg          Running
swgw-db-*              Running
cerbos                 Running
zitadel                Running
zitadel-setup          Completed

coordinator            CrashLoopBackOff
worker                 Waiting

Current Active Problem
Coordinator issuer mismatch

Current coordinator error:

authentication is enabled but not usable:

issuer mismatch

configured issuer:
https://id-swgw-nprd.example.com

reported issuer:
http://id-swgw-nprd.example.com

Evidence

Coordinator expects:

SWGW_AUTH_ISSUER
=
https://id-swgw-nprd.example.com


ZITADEL reports:

TLS enabled     : false
External Secure : false

Management Console URL:
http://id-swgw-nprd.example.com:443/ui/console


Despite environment containing:

ZITADEL_EXTERNALSECURE=true
ZITADEL_EXTERNALPORT=443

Why Coordinator Fails

Coordinator validates OIDC issuer.

Expected:

https://id-swgw-nprd.example.com


Actual:

http://id-swgw-nprd.example.com


Because schemes differ:

https != http


Coordinator exits intentionally.

Most Likely Remaining Root Cause

One of:

Possibility A

ZITADEL v4.17.3 no longer honors:

ZITADEL_EXTERNALSECURE

Possibility B

Current startup arguments:

args:
  - start
  - --masterkeyFromEnv
  - --tlsMode
  - disabled


force issuer generation as:

http://...


ignoring:

ZITADEL_EXTERNALSECURE=true

Possibility C

Chart variable names are outdated for ZITADEL 4.17.3.

Tomorrow's First Steps
Verify OIDC discovery document
kubectl port-forward -n wst2m0uspa0001c svc/zitadel 8080:8080


From another terminal:

curl http://localhost:8080/.well-known/openid-configuration


Check:

"issuer"


Expected current result:

"http://id-swgw-nprd.example.com"

Check ZITADEL docs for v4.17.3

Focus on:

EXTERNALDOMAIN
EXTERNALPORT
EXTERNALSECURE
tlsMode
issuer generation

Do NOT touch

Leave alone:

Database
CloudNativePG
DSN configuration
/tmp mounts
imagePullSecrets
enableServiceLinks
Cerbos


All of those are now confirmed working.

Final Status Tonight
PostgreSQL      ✅ Healthy
Cerbos          ✅ Healthy
ZITADEL         ✅ Running
Seeder Job      ✅ Completed
Flux            ✅ Deploying new charts

Coordinator     ❌ Issuer mismatch
Workers         ⏳ Waiting on coordinator


Single remaining blocker: ZITADEL advertises http://id-swgw-nprd.example.com while Coordinator requires https://id-swgw-nprd.example.com.


## Architecture / Repository Refactor
Rework the deployment artifacts to follow a GitOps-first structure similar to:
```
cnf-gitops/
├── clusters/
│   └── <cluster>
│       ├── 0-sources/
│       ├── 1-instances/
│       ├── kustomization.yaml
│       └── sync.yaml
│
├── instances/
│   └── <instance>
│       ├── 0-vaultauth/
│       ├── 1-secrets/
│       ├── release/
│       ├── values/
│       ├── kustomization.yaml
│       └── namespace.yaml
│
└── software/
    ├── base/
    ├── patch/
    └── schema/
```






Good idea. Add this as a separate section in the retrospective.

Future Architecture / Repository Refactor
Goal

Rework the deployment artifacts to follow a GitOps-first structure similar to:

cnf-gitops/
├── clusters/
│   └── <cluster>
│       ├── 0-sources/
│       ├── 1-instances/
│       ├── kustomization.yaml
│       └── sync.yaml
│
├── instances/
│   └── <instance>
│       ├── 0-vaultauth/
│       ├── 1-secrets/
│       ├── release/
│       ├── values/
│       ├── kustomization.yaml
│       └── namespace.yaml
│
└── software/
    ├── base/
    ├── patch/
    └── schema/

Problems With Current Layout

The current repository evolved from a Docker Compose based deployment and now supports:

Docker Compose
Helm
FluxCD
AKS


However configuration ownership is spread across multiple locations:

templates/
files/config/
values.yaml
compose files
cluster overlays


which makes it increasingly difficult to:

identify source of truth
onboard new engineers
support multiple environments
support multiple tenants/namespaces
understand ownership boundaries
Target Design Principles
1. Single Source Of Truth

All platform configuration should exist once.

Avoid:

Docker-compose version
Helm version
Flux version


of the same configuration.

Instead:

Common source
      ↓
Docker Compose
Helm
Flux


should consume the same files.

2. Centralized Configuration

Product definitions:

products/


Secrets inventory:

secrets/


Users:

users/


Roles:

access/


Identity configuration:

identity/


should remain centralized and not be duplicated per deployment mechanism.

3. Docker Compose Must Continue Working

Current local workflows depend heavily on:

docker compose up


Future refactoring must preserve:

Local developer experience


without requiring:

AKS
Flux
Vault


for development.

4. Flux Must Use The Same Artifacts

The following should remain true:

Same products.yaml
Same users.yaml
Same roles.yaml
Same secrets inventory


used by:

docker compose
helm install
flux reconciliation


No environment-specific copies of application configuration.

Only deployment-layer differences should vary.

5. Task Runner Must Continue Working

Current task automation should continue to function:

task build
task test
task seed
task deploy
task compose-up


Any new directory structure must not require maintaining separate task definitions for:

Compose
Helm
Flux

Desired Ownership Model
software/

Contains:

Application artifacts
Charts
Images
Common config
Schemas

instances/

Contains:

Namespace specific values
Secrets references
Vault auth
Environment overrides
Release definitions

clusters/

Contains:

Flux sync objects
Sources
Cluster level configuration

Non-Negotiable Requirements
Must remain centralized

Configuration should continue to be shared between:

Docker Compose
Helm
Flux


No duplication.

Must remain deployable locally

The following must continue to work:

docker compose up


without requiring:

AKS
Flux
Cloud infrastructure

Must remain GitOps friendly

The following must continue to work:

flux bootstrap
flux reconcile


without manual intervention.