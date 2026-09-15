# Security

## Reporting a vulnerability

Report privately — open a [security advisory][advisory] on this repository, or
contact the maintainers directly. Please do not open a public issue for a
vulnerability.

Include what you did, what happened, and what you expected. A proof of concept
helps; a scanner's output on its own usually does not.

[advisory]: https://github.com/abhijeet-oxide/softwareGateway/security/advisories/new

## What this software handles

It moves OCI artifacts between registries and holds the credentials to do so. It
does not hold the artifacts: blobs stream registry to registry and never touch a
worker's disk or the database.

Credentials reach a process as files in a mounted directory
(`/etc/softwaregateway/secrets/<name>/<key>`), produced from
`config/secrets/secrets.yaml` by whichever backend the deployment runs. **No
credential value is ever committed to this repository**, and the tests fail on a
product that references one the inventory does not declare.

## What the deployment does by default

- Every container runs non-root, with a read-only root filesystem, no privilege
  escalation and every Linux capability dropped. The two nginx tiers are the
  documented exception — they bind port 80 and write at container start — and
  hold no credential and terminate no TLS.
- Workers have no writable volume, so a blob cannot be buffered to disk.
- The coordinator verifies tokens offline against the identity provider's keys
  and checks the tenant, so a token minted for another organization at the same
  issuer is rejected rather than read.
- Authorization is a separate decision point (Cerbos) reading policies from
  `config/access/policies`, reviewed in pull requests like any other change.
- `networkPolicy.enabled` restricts who can reach the coordinator's API at all.
  It is off by default because a policy naming the wrong namespace is an outage.

## Supported versions

The current release. Fixes are published as a new chart version; there is no
backport branch.
