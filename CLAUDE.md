# Working in this repository

## Commands

```sh
task --list          # everything, with what each is for
task check           # what CI runs: fmt, vet, lint, test
task run             # the binaries locally, SQLite, no sign-in
docker compose up -d # the whole stack, seeded
```

Deployment changes need three more:

```sh
task chart:stage            # the chart carries a copy of config/; refresh it
task flux:build             # every kustomization under deploy/flux
task chart:template -- lab  # renders an instance exactly as Flux will
```

`helm` and `kustomize` must be on PATH for the deployment tests; without them
those tests skip rather than fail. Several shell out to subprocesses, which Go's
test cache cannot see — use `go test -count=1 ./deploy/...` when checking that a
deployment change is caught.

## Layout

```
cmd/          the three binaries
internal/     everything they are built from
pkg/          the two packages other products import (authz, apis)
config/       CONTENT: products, people, roles, Cerbos policies, secret names
deploy/
  charts/     the Helm chart. config/ is copied in at package time.
  flux/       clusters / instances / software
  examples/   a values file per situation
docs/design/  one document per decision, with its alternatives
```

## Rules that changes are held to

**Configuration is not code.** `config/` is read unchanged by `task run`,
`docker compose` and the chart. Adding a product or a person is a change to one
file there — no template, no values file, no rebuild.

**A deployment differs in one file.** Everything cluster-specific lives in
`deploy/flux/instances/<instance>/values/values.yaml`. The chart holds every
default; restating one in an instance is a test failure. If deploying somewhere
new needs a template edited, the template is wrong.

**No credential is ever a value in this repository.** `config/secrets/secrets.yaml`
lists names, keys and a path. The tests fail on a product that references a
credential the inventory does not declare.

**Ordering is explicit.** Every workload has a `wait-for-<dependency>` init
container, so a pod whose dependency is not ready sits in `Init` and says what it
is waiting for. Nothing in this deployment is allowed to reach CrashLoopBackOff
to discover a dependency.

## Style

Comments say what a reader cannot see. Do not annotate the obvious, and do not
restate the identifier. The place to be descriptive is `deploy/examples/` and the
chart's own `values.yaml`, which are read by somebody deciding what to set.

A test earns its place by failing for a real mistake and naming the fix.
`deploy/delivery_test.go` is the model: each test's comment names the incident it
prevents.

Record decisions in `docs/design/`, including the alternative and what would
change our mind. A decision that lives only in a pull request comment is one that
gets re-litigated.

## What not to do

- Do not commit anything under `deploy/charts/software-gateway/files/` — it is
  staged by `task chart:stage` and gitignored.
- Do not add a second copy of anything in `config/`. One directory, three
  runtimes.
- Do not put a company, cluster, namespace or registry name in this repository.
  Examples use `example.com`, `example.internal` or `contoso`.
