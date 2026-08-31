# Anchore Integration Guide for ORB Image Analysis and Vulnerability Retrieval


## ORB-to-Anchore Concept Mapping

| Software Gateway / ORB concept | Anchore concept | Example |
|---|---|---|
| ORB package name | Application | `application-name` |
| ORB package version | Application Version | `application-version` |
| Replicated container image | Image artifact | `registry.example.com/cfx/sbc:25.7.2` |
| Immutable image identity | Image digest | `sha256:<digest>` |
| Package component inventory | SBOM | SPDX, CycloneDX, or native format |
| Package vulnerability view | Application Version vulnerabilities | Consolidated package-level view |
| Image vulnerability view | Image vulnerabilities | Artifact-specific findings |

Recommended hierarchy:

```text
Application: application-name
└── Version: application-version
    ├── Image: sbc:25.7.2      -> sha256:<digest-a>
    ├── Image: hss:25.7.1      -> sha256:<digest-b>
    └── Image: ncom:25.7.4     -> sha256:<digest-c>
```

Use the immutable digest as the canonical image identity. Retain the repository and tag as display and traceability metadata.

---

## End-to-End Workflow

```text
ORB metadata and image index
            |
            v
Replicate images to JFrog
            |
            v
Resolve and record immutable image digests
            |
            v
Submit each image to Anchore
            |
            v
Find or create Application using ORB name
            |
            v
Find or create Version using ORB version
            |
            v
Associate analyzed images with the Version
            |
            v
Read back and validate associations
            |
            +-----------------------------+
            |                             |
            v                             v
Application Version vulnerabilities   Image vulnerabilities
            |                             |
            +--------------+--------------+
                           v
                 Normalize and enrich
                           |
                           v
             Store consolidated report
```

### Recommended processing order

1. Validate package metadata and image inventory.
2. Confirm each image exists in the target registry.
3. Resolve the digest for every image.
4. Submit images to Anchore.
5. Poll image analysis status.
6. Stop association for images whose analysis failed.
7. Find or create the Application.
8. Find or create the Application Version.
9. Associate successfully analyzed images.
10. Read associations back from Anchore.
11. Compare actual associations with the expected image index.
12. Retrieve application-version vulnerabilities.
13. Retrieve vulnerabilities for each associated image.
14. Normalize, correlate, and enrich the results.
15. Record failures, partial completion, and traceability metadata.

---

## 4. Prerequisites

Before calling Anchore:

- The target registry must be reachable by Anchore.
- Anchore must have the required registry configuration and permissions.
- Each expected image must be present in the target repository.
- The ORB name and ORB version must be known.
- The expected image list must be available.
- Preferably, the immutable digest of each image must be known.
- Authentication material must be supplied through a secrets manager or protected environment variables.
- Credentials and tokens must never be hardcoded in source code, workflow YAML, logs, or generated reports.

Suggested input model:

```json
{
  "orb_name": "application-name",
  "orb_version": "application-version",
  "images": [
    {
      "registry": "registry.example.com",
      "repository": "cfx/sbc",
      "tag": "25.7.2",
      "digest": "sha256:<digest>"
    }
  ]
}
```

---

## 5. Submit Images for Analysis

After replication, each image must be submitted to Anchore for analysis.

### API capability

```http
POST /images
```

The exact request schema must be taken from the Anchore 5.22 OpenAPI specification. The submitted image reference must identify an image that Anchore can pull from the configured registry.

### Expected integration behavior

- Submit the image once using its canonical reference.
- Treat an already-known image as an idempotent success condition when supported by the returned response.
- Capture the Anchore image identifier and digest.
- Do not log registry credentials or authorization headers.
- Record the request correlation ID if the deployment exposes one.

### Suggested local tracking fields

```text
orb_name
orb_version
registry
repository
tag
digest
anchore_image_id
submission_status
analysis_status
submitted_at
completed_at
error_code
error_message
```

---

## 6. Monitor Image Analysis

Image analysis is not assumed to be complete immediately after submission.

### API capability

```http
GET /images/{digest}
```

Poll the image record and inspect the analysis-status field defined by the 5.22 response schema.

### Control rules

- Continue only when the API reports successful analysis completion.
- Stop polling when a failure or other terminal state is returned.
- Apply a bounded retry policy with backoff.
- Treat network failures and server errors as retryable only according to the integration policy.
- Record the final Anchore status and error details.
- Do not associate a failed or incomplete image unless the business workflow explicitly permits a partial application version.

### Partial-analysis decision

If some images succeed and others fail, record the Application Version as partial in the calling system. The caller should decide whether to:

- block the package,
- retry failed images,
- associate only successful images and mark the result incomplete, or
- roll back the version association.

---

## 7. Find or Create the Application

The Anchore Application represents the ORB package.

```text
ORB name -> Anchore Application name
```

### API capabilities

```http
GET  /applications
POST /applications
```

Use the exact 5.22 OpenAPI operation that supports lookup and creation in the deployed Anchore environment.

### Idempotent behavior

1. Search for an Application matching the ORB name.
2. If exactly one matching Application exists, reuse its identifier.
3. If none exists, create it.
4. If multiple ambiguous matches exist, stop and report the conflict instead of selecting one silently.
5. Store the Anchore Application ID in Software Gateway metadata.

### Naming rule

```text
Application name = ORB package name
```

Example:

```text
application-name
```

---

## 8. Find or Create the Application Version

The Anchore Application Version represents the ORB package version.

```text
ORB version -> Anchore Application Version name
```

### API capability pattern

```http
GET  /applications/{application_id}/versions
POST /applications/{application_id}/versions
```

Confirm the exact endpoint and body in the supplied OpenAPI file.

### Idempotent behavior

1. List or find versions under the selected Application.
2. Match the ORB version exactly.
3. Reuse the existing Version ID when present.
4. Create a new version when absent.
5. Do not create duplicate versions during retries.
6. Persist the Anchore Version ID with the ORB record.

Example:

```text
Application: application-name
├── Version: 25.7_mp2604pp2
├── Version: application-version
└── Version: 25.8_mp2701
```

---

## 9. Associate Images with the Application Version

Associate each successfully analyzed image with the matching Anchore Application Version.

### API capability pattern

```http
POST /applications/{application_id}/versions/{version_id}/artifacts
```

The precise identifier type and request body must be confirmed from the OpenAPI schema. The association may require an Anchore artifact identifier, digest, or typed resource structure.

### Association rules

- Associate by immutable identity whenever supported.
- Do not rely only on a mutable tag.
- Make the operation idempotent.
- Avoid duplicate associations.
- Record the result for each image independently.
- Preserve the ORB-to-image mapping in the Software Gateway database.

Expected structure:

```text
Application
└── Version
    ├── Image digest A
    ├── Image digest B
    └── Image digest C
```

---

## 10. Verify Image Associations

Never assume that a successful write response alone proves the final state. Read the Application Version artifacts back from Anchore.

### API capability pattern

```http
GET /applications/{application_id}/versions/{version_id}/artifacts
```

### Reconciliation logic

Compare the expected image digest set with the actual associated digest set.

```text
missing    = expected - actual
unexpected = actual - expected
matched    = expected intersection actual
```

A version is fully associated only when:

```text
missing is empty
AND
unexpected is empty, unless explicitly allowed
AND
all expected images completed analysis successfully
```

Recommended reconciliation output:

```json
{
  "expected": 3,
  "associated": 3,
  "matched": 3,
  "missing": [],
  "unexpected": [],
  "status": "complete"
}
```

Do not calculate counts from tags alone. Resolve associations by immutable identity wherever the API response permits it.

---

## 11. Retrieve Image Vulnerabilities

Image-level retrieval provides artifact-specific vulnerability information.

### API capability

```http
GET /images/{digest}/vuln/{type}
```

The supported value of `{type}` and the exact response schema must be confirmed from the OpenAPI contract.

### Use image-level results for

- Identifying the affected image.
- Identifying the affected package within that image.
- Image-owner remediation.
- Image-specific reporting.
- Troubleshooting differences in application-level aggregation.
- Enriching package, fix-version, CVSS, advisory, and data-source details when present in the response.

For an ORB containing multiple images, call the image vulnerability API for every successfully analyzed and associated image.

---

## 12. Retrieve Application Version Vulnerabilities

Application-level retrieval provides a package-oriented view across artifacts associated with the Application Version.

### API capability pattern

```http
GET /applications/{application_id}/versions/{version_id}/vulnerabilities
```

Confirm the exact route, parameters, pagination behavior, and returned schema in the Anchore 5.22 OpenAPI specification.

### Use application-level results for

- ORB-level security reporting.
- Release and compliance summaries.
- Aggregated views across associated artifacts.
- Package-level vulnerability status.

Application-level data must not automatically replace the image-level dataset. The two views should be collected and compared because their scopes and aggregation behavior may differ.

---

## 13. Recommended Enrichment Strategy

Use both retrieval paths:

```text
Application Version vulnerabilities
                  +
Per-image vulnerabilities
                  |
                  v
Normalize -> Correlate -> Enrich -> Report
```

### Recommended correlation key

Do not deduplicate by CVE alone. Where available, correlate using a composite identity such as:

```text
vulnerability_id
+ image_digest
+ package_name
+ package_version
+ package_type
+ package_path or namespace
```

A single CVE may affect multiple packages or images, and each occurrence may require separate remediation evidence.

### Preserve source fields

Keep the raw values from each API view, including:

- application-level severity,
- image-level severity,
- vendor severity,
- upstream or NVD severity,
- CVSS vectors and scores,
- fix version,
- package identity,
- image digest,
- advisory or feed source,
- exploitability fields when present,
- timestamps returned by the API,
- raw source payload reference.

Do not overwrite one source value with another during normalization.

---

## 14. Severity Handling

Earlier integrations observed different severity values for what appeared to be the same CVE entry. This must be revalidated against the Anchore 5.22 response schemas and actual deployment responses.

Potentially distinct values may represent different sources or contexts, such as vendor-provided data and upstream vulnerability data. The precise meaning of every field must be determined from the response schema rather than inferred from its name.

### Recommended consumer behavior

1. Preserve every severity field with its source.
2. Preserve all CVSS scores and vectors independently.
3. Do not silently collapse conflicting values.
4. Define a documented reporting precedence only after validating the 5.22 schema and organizational policy.
5. Show the selected reporting severity together with its source.
6. Retain alternate severities for audit and troubleshooting.

Suggested normalized model:

```json
{
  "vulnerability_id": "CVE-YYYY-NNNN",
  "reported_severity": "High",
  "reported_severity_source": "<validated-source>",
  "severity_observations": [
    {
      "source": "<source-a>",
      "severity": "High",
      "score": 8.1,
      "vector": "<vector>"
    },
    {
      "source": "<source-b>",
      "severity": "Critical",
      "score": 9.8,
      "vector": "<vector>"
    }
  ]
}
```

The example structure is a proposed consumer model, not an assertion of exact Anchore field names.

---

## 15. Additional Security and Metadata APIs

### Image SBOM

```http
GET /images/{digest}/sboms/native-json
GET /images/{digest}/sboms/spdx-json
GET /images/{digest}/sboms/cyclonedx-json
```

Use SBOM responses for component inventory, license review, dependency reporting, and correlation with vulnerability findings.

### Policy evaluation

```http
GET /images/{digest}/check
```

Use policy results for security gates and release decisions. Confirm the required policy identifier, tag, or evaluation parameters from the OpenAPI definition.

### VEX

```http
GET /images/{digest}/vex/openvex
```

Use VEX-related data as a distinct input to exploitability and remediation workflows. Do not remove a vulnerability solely because a VEX record exists. Apply the organization's validated VEX policy.

### Stateless or one-time scan

```http
POST /scan
POST /vulnerability-scan
```

Use these only when their documented behavior and data-retention characteristics match the workflow. They are not automatically equivalent to submitting and associating a stored image.

---

## 16. Idempotency and Retry Design

Every stage should be safe to retry.

| Stage | Idempotency key or check |
|---|---|
| Image submission | Image digest |
| Application creation | Exact ORB name mapped to stored Application ID |
| Version creation | Application ID plus exact ORB version |
| Image association | Version ID plus immutable image identity |
| Vulnerability retrieval | Digest or Version ID plus query parameters |

### Retry guidance

- Retry transient transport failures and eligible server responses using bounded exponential backoff.
- Honor `Retry-After` when returned.
- Do not retry authentication or authorization failures without correcting credentials or permissions.
- Re-read state before retrying a create or association operation.
- Persist per-image progress so a partial batch does not restart completed work.
- Use limited concurrency for image operations and make the worker count configurable.
- Redact tokens, passwords, cookies, and authorization headers from logs.

---

## 17. Pagination and Completeness

List and vulnerability APIs may paginate results. The caller must inspect the exact 5.22 response and parameter schemas and continue until no additional page remains.

Completeness controls:

- Record the requested page size.
- Follow the documented continuation mechanism.
- Guard against repeated continuation tokens or pages.
- Record the total returned by the API when explicitly provided.
- Do not infer a total from the first page.
- Store a retrieval-complete flag only after all pages are processed.

---

## 18. Failure Handling

| Failure | Recommended action |
|---|---|
| Image unavailable in registry | Stop that image, verify replication and registry path |
| Registry authentication failure | Stop and correct Anchore registry configuration |
| Analysis failure | Record terminal details and block or mark partial according to policy |
| Application conflict | Stop and require deterministic ID resolution |
| Version conflict | Re-read versions and resolve using exact stored identifiers |
| Association failure | Retry only after checking current association state |
| Missing association | Reconcile expected versus actual, then associate the missing image |
| Unexpected association | Flag for investigation before reporting the version as complete |
| Vulnerability API pagination failure | Mark the dataset incomplete and resume from persisted state when possible |
| Response-schema mismatch | Preserve the raw response and fail validation visibly |

---

## 19. Validation Checklist

### Before analysis

- [ ] ORB name is present.
- [ ] ORB version is present.
- [ ] Expected image inventory is present.
- [ ] Every image exists in the target registry.
- [ ] Every image digest is resolved.
- [ ] Anchore registry access is configured.

### During analysis

- [ ] Every image submission result is recorded.
- [ ] Every image reaches a documented terminal status.
- [ ] Failed images retain error details.
- [ ] No credentials appear in logs.

### During application mapping

- [ ] Application lookup is deterministic.
- [ ] Application ID is persisted.
- [ ] Version lookup is deterministic.
- [ ] Version ID is persisted.
- [ ] Only intended images are associated.

### Before vulnerability reporting

- [ ] Associations have been read back.
- [ ] Expected and actual digest sets have been reconciled.
- [ ] All application vulnerability pages have been fetched.
- [ ] All image vulnerability pages have been fetched.
- [ ] Severity values retain their sources.
- [ ] Raw responses or traceable references are retained.
- [ ] Partial or incomplete data is labeled explicitly.

---

## 20. Recommended Output Structure

```text
report/
├── manifest.json
├── application.json
├── application-version.json
├── associations.json
├── association-reconciliation.json
├── vulnerabilities/
│   ├── application-vulnerabilities.json
│   ├── image-vulnerabilities/
│   │   ├── <digest-a>.json
│   │   └── <digest-b>.json
│   └── consolidated-vulnerabilities.json
├── sboms/
│   ├── <digest-a>.spdx.json
│   └── <digest-b>.spdx.json
└── errors.json
```

Suggested `manifest.json` fields:

```json
{
  "anchore_version": "5.22.0",
  "orb_name": "application-name",
  "orb_version": "application-version",
  "application_id": "<id>",
  "version_id": "<id>",
  "association_status": "complete",
  "vulnerability_retrieval_status": "complete"
}
```

---

## 21. Agent Navigation Map

An implementation agent can use this task-to-capability map before reading the full OpenAPI file.

| Goal | Start with | Then inspect in OpenAPI |
|---|---|---|
| Submit an image | `POST /images` | Request body, authentication, response image identifier |
| Check analysis | `GET /images/{digest}` | Status field and terminal values |
| Find an Application | `GET /applications` | Filter and pagination parameters |
| Create an Application | `POST /applications` | Required request fields and conflict response |
| Find or create a Version | Application Version operations | Exact path, body, returned Version ID |
| Associate an image | Application Version artifact operation | Artifact type and accepted identifier |
| Verify associations | Application Version artifact list | Returned image identity and pagination |
| Get image vulnerabilities | `GET /images/{digest}/vuln/{type}` | Supported types, fields, pagination |
| Get application vulnerabilities | Application Version vulnerability operation | Exact route, aggregation schema, pagination |
| Fetch SBOM | `/images/{digest}/sboms/*` | Media type and response format |
| Run policy check | `/images/{digest}/check` | Policy parameters and result schema |
| Fetch VEX | `/images/{digest}/vex/openvex` | Response format and status semantics |

---

## 22. Implementation Decision Summary

For the Software Gateway ORB workflow:

- Treat the ORB name as the Anchore Application name.
- Treat the ORB version as the Anchore Application Version.
- Submit all replicated images for Anchore analysis.
- Track images by digest, not only by tag.
- Associate only successfully analyzed images.
- Verify associations by reading them back.
- Retrieve vulnerabilities both at Application Version level and image level.
- Preserve source-specific severity and scoring fields.
- Do not deduplicate by CVE alone.
- Make all create, submit, and association operations idempotent.
- Label partial analysis or incomplete pagination explicitly.
- Use the Anchore 5.22 OpenAPI specification as the final authority for endpoint and schema details.

---

## References

- [Anchore Enterprise 5.22 API Reference](https://docs.anchore.com/5.22/docs/api/reference)
- Local API contract: `anchore_5.22_openapi.json`
