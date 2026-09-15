# software/patch - which release of the software an instance runs

One directory per chart version. Each is two files: `base`, and the patch that
pins `spec.chart.spec.version` on both HelmReleases.

An instance selects one with a single line in `release/kustomization.yaml`, so
lab running 0.1.0 while production runs 0.0.9 is visible in two files and is a
one-line pull request to change.

```
software/patch/
  0.1.0/
    kustomization.yaml
    version.yaml
```

A new version is added by `task flux:version -- <version>`, which also repoints
the instance you name. The release pipeline runs the same command, so what CI
does and what a developer does are one code path.
