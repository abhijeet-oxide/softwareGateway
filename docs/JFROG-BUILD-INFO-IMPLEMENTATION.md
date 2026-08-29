# JFrog Build Info After OCI Transfer

## Purpose

Implement optional JFrog Build Info publication for a successfully completed
OCI transfer. The feature associates every published component version with a
JFrog build, then publishes a Build Info document that renders component
modules, manifests, configs, and OCI layers in Artifactory.

This document is implementation-ready and intentionally does not require a
connection to JFrog. It records the API behavior already proved against a JFrog
Artifactory instance.

## Scope

The feature applies only to target repositories with `type: jfrog` or
`type: artifactory` and `enableBuild: true`.

It runs only after a transfer reaches `succeeded`. A Build Info failure must
not change a successful artifact transfer into `failed`.

The feature must support package trees containing:

- OCI images
- OCI Helm charts
- OCI file or generic artifacts
- OCI indexes and signatures as release-level metadata

## Configuration

Add this field to the target repository configuration:

```yaml
spec:
  targets:
    - name: internal-jfrog
      registry: artifactory.example.com
      repository: docker-stage
      type: jfrog
      credentialsRef:
        secretName: internal-jfrog
      enableBuild: true
```

Rules:

- `enableBuild` defaults to `true`.
- It is invalid on a non-JFrog target. Configuration validation must reject it
  rather than silently ignore it.
- The existing repository credential is used for both OCI writes and Build
  Info/property API calls. Do not add a second credential configuration.
- The credential requires Artifactory permissions to deploy, annotate
  properties, and create/read Build Info. Xray scanning permissions are a
  separate administration concern.

## Build Identity

Every target transfer gets one stable Build Info identity:

```text
build.name   = <orb_name>
build.number = <orb_version>
```

The transfer ID is preferred for `build.number` because it is immutable,
globally unique, and makes retries update the same Build Info record. A
human-readable release version belongs in Build Info properties, not in the
identity used for idempotency.

Persist the build name, build number, and association/publish state before
any outbound Build Info request. A retry must reuse the exact same name and
number.

## Lifecycle

1. The ordinary transfer copies blobs, manifests, and tags exactly as it does
   today.
2. Once all transfer jobs are terminal and none failed, the transfer becomes
   `succeeded`.
3. In the same database transaction, enqueue a durable `publish_build_info`
   outbox record if the target has `enableBuild: true`.
4. A retryable publisher processes the outbox record.
5. The publisher associates build properties with each published component
   directory.
6. The publisher sends the Build Info JSON.
7. It records `published`, the publish timestamp, and any response/request ID.

The transfer state remains `succeeded` if the publisher is unavailable. The
Build Info record has its own state: `pending`, `publishing`, `published`, or
`failed`.

## Why Properties Are Applied To A Directory

For OCI repositories, applying `build.name` and `build.number` to only
`manifest.json` does not establish the deployment association used by the
Artifactory UI. The properties must be written to the component version
directory, which owns `manifest.json`, config, and layer entries.

Example component coordinate:

```text
registry namespace/name:version
```

Example Artifactory storage directory, relative to the repository key:

```text
namespace/name/version
```

Apply properties to that directory, not to an individual layer file:

```http
PUT /artifactory/api/storage/<repository-key>/<namespace>/<name>/<version>?properties=build.name%3D<build-name>%3Bbuild.number%3D<build-number>
Authorization: Basic <credential>
```

Expected response:

```http
HTTP/1.1 204 No Content
```

## Promoting A Build

Once Build Info has been published and its artifact associations resolve, the
whole build can be promoted in one request. Do not promote each image, chart,
or file separately when the desired release is already represented by one
Build Info record.

Promotion is addressed by the exact Build Info name and number:

```http
POST /artifactory/api/build/promote/<build-name>/<build-number>
Authorization: Basic <credential>
Content-Type: application/json
```

Example:

```bash
curl --request POST \
  --url 'https://artifactory.example.com/artifactory/api/build/promote/release-family/transfer-id' \
  --user "$JFROG_AUTH" \
  --header 'Content-Type: application/json' \
  --data '{
    "status": "production",
    "comment": "Promote completed OCI release",
    "ciUser": "softwareGateway",
    "targetRepo": "oci-production",
    "sourceRepos": [
      "oci-stage"
    ],
    "dryRun": false,
    "failFast": true
  }'
```

The request must use the same `build-name` and `build-number` that were used
for directory association and Build Info publication. `targetRepo` is the
destination Artifactory repository key. `sourceRepos` restricts promotion to
the repository where this build was deployed.

### Promotion Properties

Promotion properties are useful for release metadata applied to the promoted
build, for example an environment or release marker. They are not a substitute
for the `build.name` and `build.number` properties applied to component
directories before Build Info publication.

```json
{
  "status": "production",
  "comment": "Promote completed OCI release",
  "ciUser": "softwareGateway",
  "targetRepo": "oci-production",
  "sourceRepos": ["oci-stage"],
  "properties": {
    "release": "true",
    "environment": "production"
  },
  "dryRun": false,
  "failFast": true
}
```

Use short, stable keys and string values. Treat properties as searchable
metadata, not as a security boundary: do not put credentials, tokens, or
confidential evidence in them. The implementation must preserve the exact
Build Info name/number separately from these promotion properties.

Always set `dryRun` explicitly. Use `dryRun: true` for a preflight request. The
promotion must copy content and preserve the source; the Artifactory promotion
implementation must use copy semantics and must never silently move/delete the
source content.

Successful response:

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "messages": []
}
```

An empty `messages` array means the request was accepted. It is not a complete
audit record, so the caller must fetch Build Info afterward.

### Checking Promotion Status

Read the same Build Info resource:

```http
GET /artifactory/api/build/<build-name>/<build-number>
Authorization: Basic <credential>
Accept: application/json
```

The response is wrapped in `buildInfo`. Promotion history is in
`buildInfo.statuses[]`; it is not necessarily a top-level `status` field.

Representative response:

```json
{
  "buildInfo": {
    "name": "release-family",
    "number": "transfer-id",
    "modules": [],
    "statuses": [
      {
        "status": "production",
        "comment": "Promote completed OCI release",
        "repository": "oci-production",
        "timestamp": "2026-01-02T03:04:05.678+0000",
        "user": "build-user",
        "ciUser": "softwareGateway",
        "timestampDate": 1767323045678
      }
    ]
  },
  "uri": "https://artifactory.example.com/artifactory/api/build/release-family/transfer-id"
}
```

The status check must verify all of the following:

- `buildInfo.name` and `buildInfo.number` match the requested build.
- A `statuses` entry exists with the expected promotion status.
- Its `repository` equals the requested target repository.
- The request did not return an error message.

### Curl Status Check

```bash
curl --request GET \
  --url 'https://artifactory.example.com/artifactory/api/build/release-family/transfer-id' \
  --user "$JFROG_AUTH" \
  --header 'Accept: application/json'
```

The promotion is recorded when `buildInfo.statuses[]` contains an entry whose
`status` and `repository` match the promotion request. A `200` response from
the promotion endpoint only means the request was accepted; the subsequent
GET is the authoritative status check.

## Evidence And Xray

Evidence and Xray results are related but different:

- Promotion properties are operator-supplied labels.
- Build Info records modules, artifacts, dependencies, and promotion history.
- Evidence is provenance or attestation attached through the JFrog feature/API
  supported by the installed Artifactory version.
- Xray data is scanner output and must not be represented as hand-written
  Build Info properties.

The Build Info REST payload should carry release metadata and artifact
identity only. For evidence, first check which evidence endpoint and schema are
enabled on the deployment; JFrog versions differ. Do not invent an `evidence`
field in `/artifactory/api/build` and assume it is stored. If the platform
provides a supported Build Info evidence endpoint, call it after Build Info is
published and store the returned evidence ID/state in the outbox record.

### Xray Follow-Up

Xray is intentionally out of the first implementation scope. During testing,
the direct Build Info scan request returned:

```text
403 User is not authorized to access build info
```

The Artifactory UI can display Xray data for a build through its internal UI
API, but the supported direct scan endpoint still requires an account with the
appropriate Xray and Build Info permissions. Investigate this separately with
the JFrog administrator. Do not block Build Info publication or promotion on
Xray availability, and do not claim that an empty finding set is a clean scan.

For the later investigation, a commonly available scan endpoint is:

```http
POST /artifactory/api/xray/scanBuild
Authorization: Basic <credential>
Content-Type: application/json
```

```json
{
  "name": "release-family",
  "number": "transfer-id"
}
```

Then inspect the build in the Artifactory UI or use the supported Xray/build
status endpoint. A scan request must not be reported as successful merely
because the Build Info PUT returned `204`.

Expected Xray outcomes include:

- `2xx`: scan accepted or completed; poll the documented scan/status resource.
- `403`: the credential can access Artifactory but is not authorized to scan or
  access Build Info. Grant the required Xray and Build Info permissions.
- `404`: the endpoint is not available at that path/version, or the build is
  not visible to the account.
- `429`, `5xx`, or timeout: retry with bounded backoff.

The final Xray check must assert that results are associated with the exact
Build Info name and number and that the scan state is known (`completed`,
`failed`, or `not_scanned`). Do not interpret an empty findings list as clean
when the scan state is unavailable.

### Evidence 404 Investigation

Evidence deployment is a separate signed workflow and remains open for this
integration. Prepare Evidence may succeed while Deploy Evidence returns
`404`; that does not by itself prove that the signing key is the cause.

The Evidence Service requires all of the following:

1. The public key matching the signing private key is registered in JFrog under
   the exact alias used as DSSE `signatures[].keyid`.
2. The PAE is signed byte-for-byte as returned by Prepare Evidence.
3. The original `dsse_payload`, payload type, signature, and returned
   `post_url` are used without modification.
4. The Build Info subject exists and is visible to the Evidence Service
   account.

The local private key is never uploaded. A locally generated alias is not
automatically known by JFrog; an administrator must register the public key and
confirm the alias.

Prepare evidence for a Build Info subject:

```bash
curl --request POST \
  --url 'https://artifactory.example.com/evidence/api/v1/evidence/prepare?include_pae=true' \
  --header 'Authorization: Bearer <access-token>' \
  --header 'Content-Type: application/json' \
  --data '{
    "subject": {
      "subject_type": "build",
      "build_name": "release-family",
      "build_number": "transfer-id"
    },
    "predicate_type": "https://example.com/evidence/build-verification/v1",
    "predicate": {
      "result": "approved",
      "reason": "Build verified before promotion",
      "verified_by": "softwareGateway"
    },
    "provider_id": "softwareGateway"
  }'
```

The response supplies `post_url`, `dsse_payload`, `dsse_payload_type`, and
`pre_authentication_encoding`. Sign the complete PAE and deploy to the exact
returned URL:

```bash
curl --request POST \
  --url 'https://artifactory.example.com<post_url-from-prepare-response>' \
  --header 'Authorization: Bearer <access-token>' \
  --header 'Content-Type: application/json' \
  --data-binary '@dsse.json'
```

Do not add `/artifactory` to the Evidence URL. Do not reconstruct or edit the
timestamped `post_url`, and do not replace the generated subject digest.

Ask the JFrog administrator to confirm this mapping:

```text
DSSE keyid:      <key-alias>
Registered key:  public key matching the local signing private key
```

Local signature verification checks the key pair, but does not prove that the
public key is registered in JFrog:

```bash
openssl dgst -sha256 -verify evidence-public-key.pem \
  -signature signature.bin pae.txt
```

Diagnostic interpretation:

- `400` or a signature error: inspect the key alias, signature, PAE bytes,
  payload type, and DSSE envelope.
- `401` or `403`: inspect Evidence Service permissions or token scope.
- `404` from Deploy while Prepare succeeded: verify the exact generated
  `post_url`, Build Info subject visibility, Evidence Service routing, and key
  registration. Keep all four possibilities open.
- `200` or `201` with `verified: true`: evidence was deployed successfully.

Evidence failure must not change a successful transfer or Build Info
publication into a transfer failure. Store Evidence state separately as
`prepared`, `deployed`, or `failed`, including HTTP status and sanitized error
detail.

For an additional content check, query a known component tag in the target
repository through the OCI registry API and expect `200 OK` plus a
`Docker-Content-Digest` header. Build Info status confirms the promotion
record; the target manifest confirms that content is present.

### Promotion Failure Rules

- A build with unresolved artifact paths cannot be reliably promoted.
- A missing or mismatched Build Info name/number is a precondition failure.
- Permission failures must be reported without retrying indefinitely.
- `dryRun: true` must not copy or delete content.
- Promotion must preserve the source repository and tags.
- A successful promotion should be idempotent when repeated with the same
  build, status, source repository, and target repository.

This is intentionally repeatable. The request adds this build association and
does not remove associations created by earlier builds. A component can belong
to more than one release/build, which is normal when it is reused across
package versions.

Do not send credentials in logs, errors, fixtures, documentation examples, or
process arguments.

## Build Info Operations

The following operations are the complete first-version API surface. All
examples use placeholders and require an authenticated JFrog credential.

### 1. Attach Build Info To A Component

Attach the exact Build Info identity to the component version directory. This
directory-level association is what lets Artifactory connect the OCI image,
chart, or generic artifact to the build while preserving existing associations
when a component is reused by another build.

```bash
curl --request PUT \
  --url 'https://artifactory.example.com/artifactory/api/storage/oci-stage/namespace/component/version?properties=build.name%3Drelease-family%3Bbuild.number%3Dtransfer-id' \
  --user "$JFROG_AUTH"
```

Expected response:

```text
HTTP/1.1 204 No Content
```

Use the component directory, not only `manifest.json`, a config file, or a
layer. Repeat this request for every tagged component in the release.

### 2. Create Or Publish Build Info

Send one Build Info document after all component directories have been
associated:

```bash
curl --request PUT \
  --url 'https://artifactory.example.com/artifactory/api/build' \
  --user "$JFROG_AUTH" \
  --header 'Content-Type: application/json' \
  --data-binary '@build-info.json'
```

Expected response:

```text
HTTP/1.1 204 No Content
```

`build-info.json` must contain the same `name` and `number` used in the
directory properties. Repeating the request with the same identity is the
idempotent retry path.

### 3. Delete Build Info

Delete the Build Info record by its exact name and number:

```bash
curl --request DELETE \
  --url 'https://artifactory.example.com/artifactory/api/build/release-family/transfer-id' \
  --user "$JFROG_AUTH"
```

Expected response:

```text
HTTP/1.1 200 OK
```

Deletion removes the Build Info record, not the OCI artifacts from the
repository. The implementation must require an explicit administrative
operation for deletion; transfer cleanup must not delete Build Info silently.

### 4. Promote Build Info

Promote all artifacts associated with the Build Info in one request:

```bash
curl --request POST \
  --url 'https://artifactory.example.com/artifactory/api/build/promote/release-family/transfer-id' \
  --user "$JFROG_AUTH" \
  --header 'Content-Type: application/json' \
  --data '{
    "status": "production",
    "comment": "Promote completed OCI release",
    "ciUser": "softwareGateway",
    "targetRepo": "oci-production",
    "sourceRepos": ["oci-stage"],
    "properties": {
      "release": "true",
      "environment": "production"
    },
    "dryRun": false,
    "failFast": true
  }'
```

Expected response:

```json
{
  "messages": []
}
```

The request should use `dryRun: true` for preflight. Promotion must preserve
the source repository; the implementation must never omit the copy behavior
or silently turn this into a move.

### 5. View Build And Promotion Status

Fetch Build Info and inspect `buildInfo.statuses[]`:

```bash
curl --request GET \
  --url 'https://artifactory.example.com/artifactory/api/build/release-family/transfer-id' \
  --user "$JFROG_AUTH" \
  --header 'Accept: application/json'
```

Expected response shape:

```json
{
  "buildInfo": {
    "name": "release-family",
    "number": "transfer-id",
    "modules": [],
    "statuses": [
      {
        "status": "production",
        "repository": "oci-production",
        "comment": "Promote completed OCI release",
        "ciUser": "softwareGateway",
        "timestamp": "2026-01-02T03:04:05.678+0000"
      }
    ]
  }
}
```

Treat a promotion as confirmed only when `buildInfo.statuses[]` contains the
expected status and target repository. Also query at least one known promoted
OCI tag and require `200 OK` plus `Docker-Content-Digest`; Build Info status
alone confirms metadata, not the presence of target content.

## Obtaining Destination Paths

The transfer planner already resolves where each artifact is written. Reuse
that placement information; do not infer paths from a registry listing after
the fact.

For an OCI component with a tag, its destination directory is:

```text
<target base path>/<component repository>/<tag>
```

The target base path is the configured target `repository`. A component
repository/tag comes from `org.opencontainers.image.ref.name` on the artifact
descriptor. If an artifact lacks a tagged component reference, include it as a
release-level artifact but do not create a separate component module for it.

The publisher must use the same destination layout calculation as transfer
planning. Do not duplicate layout rules in a JFrog package.

## Build Info JSON

Publish Build Info only after all component directories have been associated.

```http
PUT /artifactory/api/build
Content-Type: application/json
Authorization: Basic <credential>
```

Minimal representative body for one OCI image:

```json
{
  "version": "1.0.1",
  "name": "release-family",
  "number": "transfer-id",
  "started": "2026-01-02T03:04:05.678Z",
  "buildAgent": { "name": "softwareGateway", "version": "<version>" },
  "agent": { "name": "softwareGateway", "version": "<version>" },
  "modules": [
    {
      "id": "component-name:component-version",
      "type": "docker",
      "properties": {
        "docker.image.tag": "registry.example.com/repository/namespace/component-name:component-version",
        "docker.image.id": "sha256:<manifest-digest>"
      },
      "artifacts": [
        {
          "name": "manifest.json",
          "type": "json",
          "sha256": "<manifest-digest-without-prefix>",
          "originalDeploymentRepo": "repository-key",
          "path": "namespace/component-name/component-version/manifest.json"
        },
        {
          "name": "sha256__<config-digest-without-prefix>",
          "type": "config",
          "sha256": "<config-digest-without-prefix>",
          "originalDeploymentRepo": "repository-key",
          "path": "namespace/component-name/component-version/sha256__<config-digest-without-prefix>"
        },
        {
          "name": "sha256__<layer-digest-without-prefix>",
          "type": "layer",
          "sha256": "<layer-digest-without-prefix>",
          "originalDeploymentRepo": "repository-key",
          "path": "namespace/component-name/component-version/sha256__<layer-digest-without-prefix>"
        }
      ],
      "dependencies": [
        {
          "id": "sha256__<layer-digest-without-prefix>",
          "sha256": "<layer-digest-without-prefix>"
        }
      ]
    }
  ],
  "properties": {
    "transfer.id": "transfer-id",
    "release.manifest.digest": "sha256:<root-digest>"
  }
}
```

Expected response:

```http
HTTP/1.1 204 No Content
```

### Module Rules

| Component kind | Module type | Module ID | Artifacts |
|---|---|---|---|
| image | `docker` | `<name>:<tag>` | manifest, config, all layers |
| chart | `helm` | `<name>:<tag>` | manifest, config, chart layer |
| file | `generic` | `<name>:<tag>` | manifest, config if present, all layers |

Layer artifacts also appear in module dependencies. This is the shape JFrog
uses to render an OCI/Docker component with drill-down layers.

Indexes and detached signatures can be retained as artifacts in a release
summary module, but must not replace component modules. They have no tagged
component directory and should not be presented as deployable images.

## Persistence

Add a durable table or extend the transfer model. A separate table is
preferred because publishing is an independent, retryable side effect.

```sql
CREATE TABLE transfer_build_info (
    transfer_id       TEXT PRIMARY KEY REFERENCES transfers(id),
    target_repo_id    BIGINT NOT NULL REFERENCES repositories(id),
    build_name        TEXT NOT NULL,
    build_number      TEXT NOT NULL,
    state             TEXT NOT NULL CHECK (state IN ('pending', 'publishing', 'published', 'failed')),
    attempts          INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT,
    published_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (target_repo_id, build_name, build_number)
);
```

For SQLite, use the repository's normal logical equivalents for timestamps and
foreign keys.

The publisher must claim pending/failed records with the same lease/retry
discipline used by other durable work. Publishing must be idempotent:

- Reapplying directory properties is safe.
- Re-sending identical `PUT /api/build` content is safe.
- A response timeout after server acceptance must be retried with the same
  identity and payload.

## Interfaces And Ownership

Keep the transfer engine generic. Introduce a narrow target-specific publisher
interface, implemented in `internal/registry/artifactory`:

```go
type BuildPublisher interface {
    Publish(ctx context.Context, release BuildRelease) error
}
```

`BuildRelease` contains only normalized release data: build identity, target
repository key, resolved component directories, manifest/config/layer digests,
and media types. It must not contain configuration secrets.

The composition root selects the publisher only when `enableBuild` is true and
the target type is JFrog/Artifactory. The Publisher receives the existing JFrog
credential and transport configuration, just as the existing Xray client does.

## Failure Policy

| Failure | Transfer state | Build Info state | Retry |
|---|---|---|---|
| Property association returns 401/403 | succeeded | failed | no automatic retry until credentials/permissions change |
| Property association returns 404 | succeeded | failed | no automatic retry; indicates a layout/ordering bug |
| JFrog 429/5xx/timeout | succeeded | failed | retry with bounded backoff |
| Build Info returns 400 | succeeded | failed | no automatic retry; retain response detail |
| Build Info returns 204 | succeeded | published | none |

Record a concise audit event for both success and failure. Do not include
Authorization values or other secrets.

## Acceptance Tests

### Unit Tests

- `enableBuild` is rejected for non-JFrog targets and defaults to false.
- A successful transfer creates one pending Build Info record.
- A failed or cancelled transfer creates none.
- Retry reuses the same build name and number.
- Component modules render image/chart/file entries with correct artifact and
  dependency counts.
- A component reused by two transfers receives both build associations; no
  prior association is removed.
- Build publication failure never changes the transfer from `succeeded`.

### JFrog Integration Test

Use a disposable JFrog repository and a single OCI image with at least one
layer.

1. Copy the image with `enableBuild: true`.
2. Wait for the transfer and Build Info record to become `published`.
3. Fetch Build Info and assert one `docker` module contains manifest, config,
   and layer records.
4. Fetch the component directory properties and assert exact `build.name` and
   `build.number` values.
5. In the Artifactory UI or supported API, assert the module artifacts resolve
   to a repository path and the build Content view shows the image release.
6. If Xray permissions are available, trigger/observe a build scan and assert
   scan results are associated with the build.
7. Promote the build with `dryRun: true` and assert no target content changes.
8. Promote the same build with `dryRun: false`, then fetch Build Info and assert
  `buildInfo.statuses[]` contains the requested status and target repository.
9. Query one promoted component tag in the target repository and assert `200 OK`
  with a digest header; verify the source tag still exists.

The integration test is the final authority for repository-path behavior. Do
not replace it with mocked assertions: the behavior depends on Artifactory's
OCI storage semantics.

## Non-Goals

- Do not invoke JFrog CLI from workers.
- Do not create a build before the transfer succeeds.
- Do not fail or roll back copied OCI content because Build Info publication
  fails.
- Do not attach properties to only individual manifest or layer files.
- Do not remove previous `build.name`/`build.number` associations when a shared
  component is reused by another release.