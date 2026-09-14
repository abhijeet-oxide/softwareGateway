# 0-secrets - what must exist before this release installs

This instance runs `secrets.backend: none`, so its Secrets are created once, by
hand, and nothing in Git holds a value.

The list comes from `values/values.yaml`, so it cannot go stale — turning single
sign-on on adds a Secret to it in the same commit:

```sh
task flux:secrets -- lab
```

Export the values first and paste the output, so nothing lands in shell history.
[`docs/install.md`](../../../../../docs/install.md) is the whole sequence.

**Keep the ZITADEL master key.** It encrypts ZITADEL's own database, and a
database whose key is gone cannot be decrypted by anything.

The database password is not on that list and never will be: CloudNativePG
generates it and publishes the connection as `<cluster>-app`.

With an operator instead — the Vault Secrets Operator, or the Azure Key Vault CSI
driver — set `secrets.backend` in `values/values.yaml` and put the operator's
`VaultAuth` (or the workload identity binding) in this directory. See
`instances/prod`.
