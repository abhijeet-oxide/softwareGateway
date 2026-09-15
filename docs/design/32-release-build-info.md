# 32 - The release, as a build

## 1. Not the feature of the same name

[29](29-jfrog-build-info.md) is a **product** feature: the Coordinator publishes
Build Info to a customer's Artifactory after a transfer it ran, describing the
components it moved. This document is about **this pipeline describing itself** -
what CD published, so a release of this product can be found, promoted and
scanned the way everything else in the organisation is.

Same Artifactory API, different subject, no shared code. `deploy/buildinfo/` does
not import `internal/`, and `internal/` does not import it; a change to one is
not a change to the other. They are separate on purpose: the product's version
runs on a Coordinator against a registry it was configured with, retries through
an outbox, and must never fail a transfer. This one runs once, in a job, and
failing the release is exactly what it should do when it cannot describe it.

## 2. What it is for

Everything this pipeline produces is addressed one artifact at a time: three
images and a chart, four tags in a registry, related only by a version number
somebody has to know to type. Nothing in Artifactory says *these four are a
release*, so nothing can answer:

- which images does chart 0.1.8 actually deploy?
- has this release been scanned, all of it?
- promote the release from staging - all of it, in one act.

A Build Info is the object that answers those, and Artifactory already has the
view, the promotion API and the Xray integration built around it.

## 3. A release is every component, not a delta

This is the rule the whole thing turns on, and the one most easily got wrong.

CD does not rebuild what is already published (§12.9.1 of [30](30-continuous-delivery.md)),
and a configuration-only release builds nothing at all. So a Build Info assembled
from *what this run did* would describe an empty release, or worse a partial one:
two images where the product has three, looking complete to anybody reading it.

So the document is assembled from **the registry**, not from the run:

1. `RELEASE_IMAGES` names the components. The `images` matrix builds them;
   `TestTheReleaseImageListMatchesTheBuildMatrix` fails when the two disagree.
2. Each is resolved at the release's image version, the chart at the chart
   version - which differ, because a configuration-only release moves the chart
   and leaves the images alone.
3. **All of them resolve or nothing is published.** A component the registry does
   not have fails the job, naming it. `TestAReleaseIsEveryComponentOrNothing`
   removes each component in turn and requires the publication to fail.

That last point is the whole design in one line. A Build Info that omits an
artifact is not a smaller truth, it is a false one: the omitted component is
exactly the one that failed to publish, and it is invisible in the view built to
show what shipped.

## 4. Attestation

`actions/attest-build-provenance` signs an in-toto/SLSA provenance statement for
each image, through Sigstore, using the job's own OIDC identity. There is no key
to hold and nothing to rotate.

It is made about the **digest**, not the tag: a tag is a pointer, and the
statement has to be about the bits. Anybody can check one without trusting this
repository:

```sh
gh attestation verify oci://<registry>/<repo>/software-gateway-worker@sha256:... \
  --repo <owner>/<repo>
```

which answers which workflow, which commit and which runner produced that digest.

Off unless `BUILD_ATTESTATION` is set, because it needs `id-token: write`, and a
permission that mints identity tokens is worth granting on purpose rather than
inheriting. `BUILD_ATTESTATION_IN_REGISTRY` additionally pushes the attestation
to the registry as an OCI referrer - separate because not every Artifactory
version serves the referrers API, and a registry that does not will fail the
step rather than ignore it.

## 5. Promotion is a decision

Publishing and promoting are different acts and are not done together. A build is
published because it was built; it is promoted because somebody decided it was
good. So `promote_to` is a manual-dispatch input and empty by default.

Artifacts are **copied, not moved**. A promotion that empties the staging
repository takes the release away from everything still pulling it - a cluster
mid-rollout, a mirror part-way through a sync. Dependencies are not promoted:
they belong to the repositories they came from, and copying a third-party base
image into a release repository claims it as this product's output.

## 6. Configuration

| Variable | Effect when unset |
|---|---|
| `BUILD_INFO` | The job does not run. Nothing else changes. |
| `BUILD_INFO_URL` | Required when `BUILD_INFO` is set: the Artifactory **root**, including `/artifactory` and nothing after it. It is usually the same machine as the registry and never the same URL - the registry host has no `/artifactory` and the API paths are appended here, so `.../artifactory/api/build` is what gets called. That mistake is what the 404 message names. |
| `BUILD_INFO_PROJECT` | The build is filed in Artifactory's **global** scope. That is accepted silently and is usually wrong: a repository named `<key>-oci-stage` belongs to project `<key>`, the build should be filed there too, and a credential scoped to the project may be refused without it. |
| `BUILD_INFO_NAME` | `software-gateway`. The Build Info name, stable across releases; the number is the chart version, so a retry updates one record instead of opening a second. |
| `BUILD_ATTESTATION` | No provenance is signed. |
| `BUILD_ATTESTATION_IN_REGISTRY` | Attestations stay in GitHub rather than being pushed as OCI referrers. |

No new secret: `REGISTRY_USERNAME` and `REGISTRY_TOKEN` already push the images,
and Build Info is published with the same credential. It does need **deploy
permission on the build**, which Artifactory grants separately from permission to
push images - a 403 here says so rather than printing the status.

## 7. What would change our mind

- **If a release ever spans two registries.** The document assumes one, and the
  `originalDeploymentRepo` of every artifact is the same repository. Two would
  mean a module-level repository, not a document-level one.
- **If the registry becomes expensive to query.** Resolving is one GET per
  component against a manifest that usually exists. If that stops being cheap the
  answer is to carry the digests forward from the `images` job as outputs, not to
  go back to describing the run instead of the release.
- **If Artifactory gains a first-class release bundle** that this organisation
  adopts. Build Info is the object its Builds view, promotion API and Xray
  integration are built around today; a release bundle would supersede it, and
  this document would become a note about what it replaced.

## 8. Files

- [`deploy/buildinfo/`](../../deploy/buildinfo/) - the model, the registry read, the Artifactory client
- [`deploy/buildinfo/cmd/buildinfo/`](../../deploy/buildinfo/cmd/buildinfo/) - the command CD runs
- [`.github/workflows/cd.yml`](../../.github/workflows/cd.yml) - the `build-info` job and the attestation step
