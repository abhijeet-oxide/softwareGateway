# Two sets of pipelines

`ci.yml`, `cd.yml` and `security.yml` are the pipelines. They run here and they
run in a fork, and they are the ones to edit.

`*_enterprise.yml.disabled` is the same pipeline written for a repository that
cannot reach the public internet. Nothing runs them from this repository: GitHub
reads `.yml` and `.yaml` under this directory and ignores every other extension,
so the suffix is what keeps them inert.

## What they are for

The enterprise copy of this repository is not a different product, it is the
same product in a network that says no to more things:

- **The repository owner has an IP allow list.** A GitHub-hosted runner is not
  on it, so `actions/checkout` fails with `403` before a job reads a line of
  code. Every job must land on the in-network self-hosted runner.
- **Actions are allow-listed.** GitHub's own, the enterprise's own, verified
  Marketplace publishers, and a named list. A third-party setup action is
  refused before the job starts (docs/design/30 section 12.10).
- **Egress is restricted.** A scanner that downloads its database, a toolchain
  fetched from a release page, and a package manager installed from npm are
  three separate things that can be blocked independently.
- **Advanced Security may not be on.** CodeQL, SARIF upload, dependency review
  and the code scanning API all need it.

The public files carry none of that, because none of it is true here and a
pipeline full of switches nobody sets is a pipeline nobody can read.

## Using one

In the enterprise repository:

```sh
cp .github/workflows/security_enterprise.yml.disabled .github/workflows/security.yml
```

Then set the repository variables the file's header lists, under
Settings -> Secrets and variables -> Actions -> Variables.

| Variable | Used by | What happens when it is unset |
|---|---|---|
| `RUNNER_LABEL` | all three | **The run fails at startup.** Deliberate: the public files fall back to `ubuntu-latest`, and in a repository with an IP allow list that fallback is a 403 dressed up as a scanner failure. |
| `TOOL_MIRROR` | ci, cd, security | Task, Helm, kustomize, kubeconform, golangci-lint and gitleaks come from github.com and get.helm.sh, as in the public files. Set it to a base URL and each archive is fetched as `<mirror>/<file name>` - the names are already versioned, so the mirror is one flat directory. |
| `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` | all three | **No proxy for anything the runner fetches.** The jobs reach vuln.go.dev, ghcr.io, the npm registry and a handful of release pages directly, and behind an egress policy that is a `Forbidden` per tool. Set them even if the runner image already carries a proxy: each workflow sets both upper and lower case from these, and an unset variable writes an empty one over the image's. |
| `GOPROXY`, `GOSUMDB` | all three | Go's own defaults (`proxy.golang.org`, `sum.golang.org`). Set `GOPROXY` to an internal module mirror. `GOSUMDB` is the next thing to fail: `go run <module>@latest`, which is how govulncheck is installed, resolves a module `go.sum` has never seen and verifies it against the checksum database - set it to `off` if the mirror is trusted and does not proxy `sum.golang.org`. |
| `BUILD_INFO` | cd | The release is not published to Artifactory's Builds view. Set it to `true` with `BUILD_INFO_URL` (the Artifactory **root**, including `/artifactory` - not the registry host) and a release becomes one object you can promote and scan. See [docs/design/32](../../docs/design/32-release-build-info.md). |
| `BUILD_INFO_NAME` | cd | `software-gateway`. The Build Info name; the number is the chart version. |
| `BUILD_ATTESTATION` | cd | No SLSA provenance is signed. Set it to `true` and each image digest gets an in-toto statement signed through Sigstore, verifiable with `gh attestation verify`. Needs no key. |
| `BUILD_ATTESTATION_IN_REGISTRY` | cd | Attestations stay in GitHub instead of being pushed to the registry as OCI referrers, which not every Artifactory serves. |
| `BUILDX_DRIVER` | cd | `docker-container`, a real BuildKit, so the registry layer cache works. Set it to `docker` on a runner that is itself a container and cannot start a privileged one - the builder otherwise dies with `error mounting "sysfs" to rootfs ... operation not permitted`. The layer cache is dropped with it. |
| `NPM_REGISTRY` | ci, cd, security | npm's default registry. Set it and `.github/actions/web-toolchain` writes an npmrc that both npm and pnpm read, so pnpm itself and every package come from the one host. |
| `SECURITY_CODEQL` | security | CodeQL does not run. It needs Advanced Security **and** egress for the bundle. |
| `SECURITY_DEPENDENCY_REVIEW` | security | Dependency review does not run. Needs Advanced Security and the dependency graph. |
| `SECURITY_TRIVY` | security | Trivy does not run. Its database comes from ghcr.io. |
| `SECURITY_GOVULNCHECK` | security | govulncheck does not run. Needs the module proxy and vuln.go.dev. |
| `SECURITY_PNPM_AUDIT` | security | `pnpm audit` does not run. A pull-through registry mirror often does not serve the audit endpoint. |
| `SECURITY_ALERT_EXPORT` | security | The alert inventory does not run. It needs the code scanning API, and `gh` and `jq` on the runner. |
| `TRIVY_DB_REPOSITORY`, `TRIVY_JAVA_DB_REPOSITORY` | security | Upstream (ghcr.io). Set them to internal mirrors. |
| `GOVULNDB` | security, **and the shared `security.yml`** | Upstream (`vuln.go.dev`). **`GOPROXY` does not cover this.** govulncheck makes two fetches: the tool is a Go module and comes through `GOPROXY`, but the vulnerability database behind it is a plain HTTPS request to `vuln.go.dev`, which no module mirror serves. Point this at a mirror of that database, allow the host, or leave govulncheck off. |

### CodeQL is a repository setting before it is a workflow

A repository has **either** CodeQL default setup **or** an advanced setup, never
both. With default setup on in Settings -> Code security, the job in
`security.yml` runs to completion and is refused at the upload:

```
Error: Code Scanning could not process the submitted SARIF file:
CodeQL analyses from advanced configurations cannot be processed when
the default setup is enabled
```

Nothing in either copy of the workflow changes that. Switch default setup off to
keep the workflow - it is the one that names the query pack, the excluded test
trees and the languages a diff is worth scanning - or keep default setup and
leave `SECURITY_CODEQL` unset in the enterprise copy, which skips the job
instead of failing it.

One more thing a self-hosted image needs: CodeQL's Go extractor reports `The
file program is required on Linux, but does not appear to be installed`. It is a
diagnostic rather than a failure, and `file` on the image clears it.

### The one secret

`REGISTRY_USERNAME` and `REGISTRY_TOKEN`, under Settings -> Secrets and
variables -> Actions -> Secrets. One Artifactory commonly serves the container
registry, the npm registry and the Go module mirror; that is one host with one
login, so it is one secret used three ways rather than three secrets.

- `cd.yml` logs the image build and the chart push in with it.
- The web toolchain writes an npmrc from it, for a registry that will not serve
  packages anonymously.
- The Go toolchain writes a **netrc** from it. This is the one that surprises
  people: **Go has no `GOPROXY_TOKEN`.** Point `GOPROXY` at an authenticated
  Artifactory with no credential configured and every fetch fails with a 401
  that names no setting to change. Go reads a netrc and nothing else, so
  `.github/actions/toolchain` derives the hosts from `GOPROXY`, `GOSUMDB` and
  `GOVULNDB`, writes one, and points `NETRC` at it.

Unset, all three read anonymously and nothing changes. Two guards are worth
knowing: a token with no username fails the step rather than writing half a
credential, and a public host among those URLs (`proxy.golang.org`,
`sum.golang.org`, `vuln.go.dev`) is skipped, so leaving `GOPROXY` at its default
never sends the organisation's token to Google.

The alternative for Go is credentials in the `GOPROXY` URL itself. It works and
it leaks: that URL is printed by `go env`, quoted back in module errors, and
inherited by every child process.

It is written to `$RUNNER_TEMP`, mode 0600, and never to a home directory: a
self-hosted runner's `HOME` outlives the job, and an `~/.npmrc` there would hand
the credential to every later job on that machine. Passing a token without
`NPM_REGISTRY` fails the step rather than writing the credential against
whatever registry the runner defaults to.

Every `SECURITY_*` variable is off unless it is exactly `true`. Off rather than
on, because a scanner that cannot reach its database does not report "nothing
found" - it fails, and a Security check that is red for a blocked egress is a
check everybody learns to scroll past.

**The secret scan is not one of them.** It reads the git history and asks no
service anything, it enforces the rule the rest of this repository rests on, and
a credential in the history is a credential to rotate whatever the network
policy says. It always runs and it always fails the build.

## Keeping them in step

`TestEnterpriseWorkflowsTrackTheirOriginals` in `deploy/delivery_test.go` fails
when a job is added to a public pipeline and not to its enterprise copy. It
compares the set of jobs and nothing else: what the jobs do is exactly what the
two files are allowed to disagree about.

A copy is a poor mechanism and it is the honest one here - the two networks
disagree about too much for one file with switches. If a deviation turns out to
be something the public file can carry unset, move it there and delete it from
the copy.
