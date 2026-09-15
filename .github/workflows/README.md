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
| `NPM_REGISTRY` | ci, cd, security | npm's default registry. Set it and `.github/actions/web-toolchain` writes an npmrc that both npm and pnpm read, so pnpm itself and every package come from the one host. |
| `SECURITY_CODEQL` | security | CodeQL does not run. It needs Advanced Security **and** egress for the bundle. |
| `SECURITY_DEPENDENCY_REVIEW` | security | Dependency review does not run. Needs Advanced Security and the dependency graph. |
| `SECURITY_TRIVY` | security | Trivy does not run. Its database comes from ghcr.io. |
| `SECURITY_GOVULNCHECK` | security | govulncheck does not run. Needs the module proxy and vuln.go.dev. |
| `SECURITY_PNPM_AUDIT` | security | `pnpm audit` does not run. A pull-through registry mirror often does not serve the audit endpoint. |
| `SECURITY_ALERT_EXPORT` | security | The alert inventory does not run. It needs the code scanning API, and `gh` and `jq` on the runner. |
| `TRIVY_DB_REPOSITORY`, `TRIVY_JAVA_DB_REPOSITORY` | security | Upstream (ghcr.io). Set them to internal mirrors. |
| `GOVULNDB` | security, **and the shared `security.yml`** | Upstream (`vuln.go.dev`). govulncheck makes two fetches and only the first uses GOPROXY: the tool is a module, the database behind it is a plain request to that host. A policy that mirrors Go modules and not that host fails the second while the first succeeds. |

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

`REGISTRY_TOKEN`, under Settings -> Secrets and variables -> Actions ->
Secrets. `cd.yml` already reads it for the image build; the enterprise copies of
`ci.yml` and `security.yml` pass it to the web toolchain as well, for a registry
that will not serve packages anonymously. Unset, the registry is read
anonymously and nothing else changes.

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
