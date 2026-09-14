# Contributing

## What you need

Go (the version in `go.mod`), Node 22 for the web tier, and
[Task](https://taskfile.dev). Docker or Podman for the compose stack. `helm` and
`kustomize` for the deployment tests — without them those tests skip rather than
fail.

```sh
task --list          # everything, with what it is for
task check           # what CI runs: build, vet, lint, test
```

## Before you push

```sh
task check
```

CI runs the same commands. If `task check` passes and CI does not, that is a bug
in the Taskfile and worth reporting.

Deployment changes need two more:

```sh
task chart:stage     # the chart carries a copy of config/; this refreshes it
task flux:build      # every kustomization under deploy/flux
task chart:template  # renders an instance exactly as Flux will
```

## The shape of a change

**Configuration is not code.** `config/` holds products, people, roles, Cerbos
policies and the credential inventory, and it is read unchanged by `task run`,
`docker compose` and the chart. Adding a product or a person is a change to one
file there and nothing else — no template, no values file, no rebuild.

**A deployment differs in one file.** Everything specific to a cluster lives in
`deploy/flux/instances/<instance>/values/values.yaml`. If a change requires
editing a template to deploy somewhere new, the template is wrong.

**Credentials are never values.** `config/secrets/secrets.yaml` lists names, keys
and a path. No file in this repository holds a credential, and the tests fail if
a product references one the inventory does not declare.

## Commits and pull requests

One change per pull request, with a subject line that says what changed rather
than which file moved. The body is for why: what the alternative was and why it
was not taken. `git log` is read years later by somebody deciding whether they
can change the thing you wrote.

CI must be green before review. It runs build, vet, `golangci-lint`, the unit
tests, the chart render for every instance, `kubeconform` against the rendered
manifests, and a set of configurations the chart is expected to **refuse** —
that last one is where a new validation rule gets its test.

## Tests

A test earns its place by failing for a real mistake and saying what to do about
it. `deploy/delivery_test.go` is the model: each test's comment names the
incident it exists to prevent, and each failure message names the fix rather than
the invariant.

Some of those tests shell out to `helm` and `kustomize`. Go's test cache cannot
see what a subprocess read, so use `go test -count=1 ./deploy/...` when checking
that a deployment change is caught.

## Documentation

`docs/design/` records decisions, including the alternatives and what would
change our mind. A decision that is only in a pull request comment is a decision
that will be re-litigated.

Comments in the chart and the deployment manifests are for what a reader cannot
see. Do not annotate the obvious.
