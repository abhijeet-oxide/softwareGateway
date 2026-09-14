# 0-secrets - what has to exist in this namespace before the release installs

This instance runs `secrets.backend: none`, so the Secrets are created once, by
hand, and nothing in Git holds a value.

```sh
NS=swgw-lab

# The registry the kubelet pulls with. Named in values.yaml as imagePullSecrets.
kubectl -n $NS create secret docker-registry acr-pull \
  --docker-server=contoso.azurecr.io \
  --docker-username="$SP_ID" --docker-password="$SP_SECRET"

# ZITADEL's own two. The master key must be EXACTLY 32 bytes, and it must be
# kept: without it the ZITADEL database cannot be decrypted.
kubectl -n $NS create secret generic swgw-zitadel \
  --from-literal=masterkey="$(head -c 32 /dev/urandom | base64 | head -c 32)" \
  --from-literal=rootPassword="$(head -c 24 /dev/urandom | base64)"
```

Every other credential is listed in `config/secrets/secrets.yaml`, which carries
names and keys and no values. Create one Secret per entry with the keys it names.

With an operator instead - the Vault Secrets Operator or the Azure Key Vault CSI
driver - set `secrets.backend` in `values/values.yaml` and put the operator's
`VaultAuth` (or the workload identity binding) here. See `instances/prod`.
