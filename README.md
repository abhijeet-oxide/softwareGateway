# softwareGateway

Gateway for replicating OCI artifacts from one repository to another.

A cloud-native platform that continuously discovers software packages published to vendor OCI registries and replicates them into internal registries — optimized for throughput on 30–60 GB packages, resilient to any single failure, and operable without reading the source.

> **Status: design phase.** This repository currently contains the system design only. No implementation yet — see [17 — Delivery Plan](docs/design/17-delivery-plan.md).

## What it does

- **Discovers** new software packages across vendor OCI repositories, continuously and without duplicates
- **Replicates** them into one or more internal registries, streaming blobs registry-to-registry without ever touching disk
- **Promotes** packages between internal registries (lab → production)
- **Verifies** vendor signatures with cosign/Sigstore, at source and at destination
- **Deduplicates** by content address, so a blob moves once no matter how many packages reference it

## Architecture

Three binaries, one PostgreSQL database, nothing else.

| Component | Role |
|---|---|
| `cmd/coordinator` | Control plane — API, discovery, scheduling, queue, notifications, audit |
| `cmd/worker` | Data plane — stateless; streams OCI blobs registry to registry |
| `cmd/transferctl` | CLI — a pure Coordinator API client |

Artifact bytes flow only between registries. They never enter the Coordinator, never land on a worker's disk, and never pass through the database.

## Documentation

**New here? [Read the Functional Overview →](docs/FUNCTIONAL-OVERVIEW.md)**

What the tool does, the logical components and where they run, the file-level code layout, the CLI grouped by task, and ten day-in-the-life scenarios showing what operating it actually looks like.

**Building it? [Read the design →](docs/design/README.md)**

Eighteen documents covering component responsibilities, the full data model and SQL schema, queue and scheduling algorithms, the transfer engine, API surface, state machines, failure recovery, observability, deployment, and technology choices — each major decision recorded with the alternatives considered and what would change our mind.

Start with [00 — Overview](docs/design/00-overview.md).

## License

TBD.
