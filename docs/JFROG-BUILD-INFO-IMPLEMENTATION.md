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

- `enableBuild` defaults to `false`.
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
build.name   = <product or release family name>
build.number = <transfer ID>
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

This is intentionally repeatable. The request adds this build association and
does not remove associations created by earlier builds. A component can belong
to more than one release/build, which is normal when it is reused across
package versions.

Do not send credentials in logs, errors, fixtures, documentation examples, or
process arguments.

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