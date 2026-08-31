# Anchore Enterprise API Navigator

> Agent-first navigation guide for Anchore Enterprise OpenAPI 5.22

This document provides a high-level map of the Anchore API so agents and developers can quickly locate relevant endpoints without reading the full OpenAPI specification.

---

# API Domain Map

```text
Anchore Enterprise
│
├── Identity & Access
│   ├── User Management
│   ├── Accounts
│   ├── API Keys
│   ├── OAuth
│   └── RBAC
│
├── Image Security
│   ├── Image Analysis
│   ├── Vulnerabilities
│   ├── SBOM
│   ├── VEX
│   ├── STIG
│   └── Policy Evaluation
│
├── Source Security
│   ├── Source Repositories
│   ├── Source SBOMs
│   ├── Source Vulnerabilities
│   └── Source Policy Checks
│
├── Applications
│   ├── Applications
│   ├── Versions
│   ├── Artifact Associations
│   └── Combined SBOMs
│
├── Supply Chain
│   ├── Artifact Relationships
│   ├── Sources
│   ├── Images
│   └── Applications
│
├── Imports
│   ├── Image Imports
│   ├── Source Imports
│   └── SBOM Imports
│
├── Runtime Inventory
│   ├── Kubernetes
│   ├── ECS
│   └── Runtime Images
│
├── Governance
│   ├── Policies
│   ├── Alerts
│   ├── Reports
│   └── Lifecycle Policies
│
├── Notifications
│   ├── Slack
│   ├── Teams
│   ├── Jira
│   ├── GitHub
│   ├── SMTP
│   └── Webhooks
│
└── System Administration
    ├── Registries
    ├── Feeds
    ├── Integrations
    ├── Configuration
    └── Health
```

---

# Quick Lookup

## Analyze an Image

- `POST /images`
- `GET /images`
- `GET /images/{digest}`
- `DELETE /images/{digest}`

## Vulnerabilities

- `GET /images/{digest}/vuln/{type}`
- `POST /vulnerability-scan`
- `GET /query/vulnerabilities`

## Policy Evaluation

- `GET /images/{digest}/check`
- `GET /sources/{source_id}/check`
- `POST /scan`

## SBOMs

### Image

- `/images/{digest}/sboms/native-json`
- `/images/{digest}/sboms/spdx-json`
- `/images/{digest}/sboms/cyclonedx-json`

### Source

- `/sources/{source_id}/sbom/native-json`
- `/sources/{source_id}/sbom/spdx-json`
- `/sources/{source_id}/sbom/cyclonedx-json`

---

# Major Categories

## Images

Primary image analysis APIs.

### Core

```http
GET    /images
POST   /images
GET    /images/{digest}
DELETE /images/{digest}
```

### Security

```http
/images/{digest}/vuln
/images/{digest}/check
/images/{digest}/vex/openvex
```

### Content

```http
/images/{digest}/content
/images/{digest}/content/files
/images/{digest}/content/java
/images/{digest}/content/licenses
/images/{digest}/content/malware
```

## Policies

```http
GET    /policies
POST   /policies
GET    /policies/{id}
PUT    /policies/{id}
DELETE /policies/{id}
```

Used by:

- Image evaluations
- Source evaluations
- Stateless scans

## Applications

Represents software products.

```text
Application
 └── Version
      ├── Images
      ├── Source Repositories
      ├── Vulnerabilities
      └── SBOM
```

Key APIs:

```http
/applications
/applications/{id}
/applications/{id}/versions
/applications/{id}/versions/{version}/vulnerabilities
```

## Sources

```http
GET /sources
GET /sources/{id}
DELETE /sources/{id}
```

Capabilities:

- Source SBOMs
- Source vulnerability reports
- Source policy checks

## Imports

Used for external SBOM ingestion.

```http
/imports/images
/imports/sources
```

Common upload artifacts:

- SBOM
- Manifest
- Dockerfile
- Secret search results
- Content search results

## Artifact Relationships

Supply-chain graph APIs.

```http
/artifact-relationships
```

Interesting endpoint:

```http
/artifact-relationships/{relationship_id}/diffs/sbom
```

Used for SBOM comparisons.

## Runtime Inventory

### Kubernetes

```http
/kubernetes-inventory
/kubernetes-pods
/kubernetes-nodes
/kubernetes-containers
```

### ECS

```http
/ecs-inventory
/ecs-services
/ecs-tasks
/ecs-containers
```

## Registries

```http
GET /registries
POST /registries
PUT /registries/{registry}
DELETE /registries/{registry}
```

## Notifications

Supported targets:

- Slack
- Teams
- Jira
- GitHub
- SMTP
- Webhook

Pattern:

```text
Configuration
   ↓
Selector
   ↓
Events
```

## User & RBAC

### Identity

```http
/user
/account
```

### OAuth

```http
/oauth/token
/oauth/revoke
```

### Roles

```http
/rbac-manager/roles
/rbac-manager/users/{username}/roles
```

## System

### Health

```http
/
/health
/status
/version
```

### Configuration

```http
/system/configurations
```

### Feeds

```http
/system/feeds
```

### Integrations

```http
/system/integrations
```

---

# 80/20 APIs

If an agent only learns a small subset of Anchore:

```http
POST /images
GET  /images
GET  /images/{digest}

GET  /images/{digest}/vuln/{type}
GET  /images/{digest}/check

GET  /images/{digest}/sboms/spdx-json
GET  /images/{digest}/sboms/cyclonedx-json

POST /scan
POST /vulnerability-scan

GET  /policies
POST /policies

GET  /sources
GET  /sources/{id}

POST /imports/images
POST /imports/sources

GET  /applications
GET  /artifact-relationships

GET  /registries

GET  /system
GET  /version
```

These APIs cover most automation, compliance, SBOM, vulnerability, and reporting workflows.
