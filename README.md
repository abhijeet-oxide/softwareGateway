# Software Gateway

[![CI](https://github.com/abhijeet-oxide/softwareGateway/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/abhijeet-oxide/softwareGateway/actions/workflows/ci.yml)
[![CD](https://github.com/abhijeet-oxide/softwareGateway/actions/workflows/cd.yml/badge.svg?branch=main)](https://github.com/abhijeet-oxide/softwareGateway/actions/workflows/cd.yml)
[![OCI Artifacts](https://img.shields.io/badge/OCI-artifacts-2496ED?logo=docker&logoColor=white)](https://opencontainers.org/)
[![Release Lifecycle](https://img.shields.io/badge/release-lifecycle-1F6FEB)](#release-workflow)
[![Compliance](https://img.shields.io/badge/compliance-governed-2EA043)](#compliance)
[![Security](https://img.shields.io/badge/security-controlled-8250DF)](#security)

Discovers software packages published to vendor OCI registries and replicates
them into internal ones — streamed registry to registry, deduplicated by content
address, and recorded.

> **Status: M3 in progress.** Packages transfer: discovery, the artifact tree,
> auto-download rules, and the bytes moving to your registry. Still to come in
> M3 — chunked-upload resumption and the pause/resume/cancel controls
> (`transferctl transfers` is read-only). No 30–60 GB acceptance run has been
> done yet. [Delivery plan](docs/design/17-delivery-plan.md).

## What it does

- **Discovers** new packages across vendor repositories, continuously and without duplicates
- **Replicates** them into internal registries, streaming blobs without touching disk
- **Promotes** between internal registries (lab → production)
- **Scans** with JFrog Xray and Anchore, reporting where the two disagree
- **Verifies** vendor signatures with cosign, at source and at destination
- **Deduplicates** by content address, so a blob moves once

## Architecture

Three binaries, one PostgreSQL database, nothing else.

| | |
|---|---|
| `cmd/coordinator` | control plane — API, discovery, scheduling, queue, audit |
| `cmd/worker` | data plane — stateless; streams OCI blobs registry to registry |
| `cmd/transferctl` | CLI — a pure Coordinator API client |

Artifact bytes flow only between registries. They never enter the coordinator,
never land on a worker's disk, and never pass through the database.

## Run it

```sh
docker compose up -d        # the whole stack, seeded — http://localhost:8000
task run                    # the binaries, SQLite, no sign-in
```

In a cluster, a deployment is **one values file** and Flux does the rest:

```
deploy/flux/instances/<instance>/values/values.yaml
```

## Documentation

| | |
|---|---|
| [Install](docs/install.md) | a cluster, start to finish: the secrets, the commands, the order |
| [Quick start](QUICKSTART.md) | every variable, adding people and products, Podman, proxies |
| [Developer guide](docs/DEVELOPER-GUIDE.md) | build, configure, test, and a worked example |
| [Functional overview](docs/FUNCTIONAL-OVERVIEW.md) | what it does, where it runs, ten day-in-the-life scenarios |
| [Design](docs/design/README.md) | thirty documents, each decision with its alternatives |
| [Troubleshooting](docs/deployment-troubleshooting.md) | the cluster failures this has hit, and what stops them now |
| [Contributing](CONTRIBUTING.md) | what CI runs, and the shape of a change |
| [Entra registration](docs/entra-app-registration.md) | single sign-on, and the three settings that fail late |

Also: [compliance](docs/compliance/README.md) — what a check may assert and how
to add one — and [Anchore](docs/security/anchore-integration.md).

## License

TBD.
