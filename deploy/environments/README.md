# environments/ - what is deployed, where, and at which version

One file per environment, and it is the whole deployment record:

```
lab/helmrelease.yaml     namespace swgw-lab,  from branch `lab`
prod/helmrelease.yaml    namespace swgw,      from branch `main`
```

`git log -p deploy/environments/prod/helmrelease.yaml` is the production
deployment history. Every line of it was written either by a person in a pull
request or by the release pipeline moving `spec.chart.spec.version`, and there
is nothing else to correlate.

## Why the values are inline

They could be a ConfigMap built by `configMapGenerator`, and that is the
arrangement most Flux examples show. Inline is better here for one reason: a
change to an inline value changes the HelmRelease object, so Flux reconciles it
**immediately**. A change to a referenced ConfigMap is picked up on the
HelmRelease's own interval, which means a value edited at 09:00 takes effect at
some point before 09:10 and the person who edited it cannot tell whether it
has. That is a bad property for the file that says which database production
talks to.

The cost is that a value shared by both environments is written twice. That is
a handful of lines, and it buys a file somebody can read top to bottom and know
what an environment is.

## What is NOT here

**Secrets.** Not one value. Everything credential-shaped is a reference to a
Secret produced in-cluster from `config/secrets/secrets.yaml` by the Vault
Secrets Operator or the Azure Key Vault CSI driver - see the chart's
`secrets.backend`. What lives here is the NAME of a Secret, which is not a
secret.

**Products, users, roles and policies.** Those are `config/`, they are carried
inside the chart, and they are the same in both environments by construction.
An environment that needs a different product list is not an environment, it is
a different deployment - and it gets its own directory here rather than a fork
of the shared one.

## Changing one

| you want to | change |
|---|---|
| deploy a new release | nothing - the pipeline moves `version` on merge |
| pin or roll back | `spec.chart.spec.version`, in a pull request |
| stop automatic deploys | `spec.suspend: true` |
| scale, resize, retarget | the value under `spec.values` |

A rollback is a pull request that sets `version` back. It is reviewed, it is in
the history, and it takes the same path as a deploy - which is what makes it
something anybody on the team can do at 02:00 rather than a `helm rollback`
that leaves the cluster disagreeing with Git.
