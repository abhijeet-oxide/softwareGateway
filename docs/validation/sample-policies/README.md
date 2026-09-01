# Artigen OPA Policies

This directory contains [Rego](https://www.openpolicyagent.org/docs/latest/policy-language/) policy files evaluated by `artigen validate` when validating Kubernetes manifests.

## How It Works

During `artigen validate`, the OPA validator loads every `.rego` file in this directory (or the directory specified by `--policies`), evaluates it against the rendered Kubernetes manifests, and reports any violations as check results.

## Policy Contract

Each `.rego` file **must**:

1. **Declare a package** under `artigen.policies.<name>`:

   ```rego
   package artigen.policies.my_policy
   ```

2. **Define a `violations` rule** returning a set of objects:

   ```rego
   violations contains violation if {
       # ... your logic ...
       violation := {
           "msg": "Human-readable violation message",
           "severity": "fail",           # "fail", "warn", or "info"
           "resource": {                  # optional
               "kind": "Deployment",
               "namespace": "my-ns",
               "name": "my-deploy",
           },
           "remediation": "Add resource limits to all containers",  # optional
       }
   }
   ```

3. **Receive input** in this shape:
   ```json
   {
     "manifests": [ ... ],
     "namespace": "target-namespace",
     "metadata": {
       "helm_releases": [ ... ],
       "helm_repositories": { ... }
     }
   }
   ```

## Severity Levels

| Severity | Meaning                                |
| -------- | -------------------------------------- |
| `fail`   | Violation that should block deployment |
| `warn`   | Potential issue worth reviewing        |
| `info`   | Informational finding                  |

## Usage

```bash
# Use built-in policies (this directory)
artigen check -n my-namespace

# Use custom policies directory
artigen check -n my-namespace --policies /path/to/policies

# Policies co-located with artifacts (auto-detected)
ls my-artifacts/policies/*.rego
artigen check -n my-namespace --repo my-artifacts
```

## Writing Your Own Policies

1. Create a `.rego` file in this directory (or your own policies dir).
2. Follow the package naming convention: `artigen.policies.<your_policy_name>`.
3. Define the `violations` rule returning structured violation objects.
4. Test with `artigen check -n <namespace> --policies .`

### Tips

- Use `input.manifests` to iterate over all Kubernetes resources.
- Filter by `kind`, `apiVersion`, or labels as needed.
- The `input.namespace` field contains the target namespace.
- Keep policies focused - one concern per file.

## Shipped Policies (16)

| File                     | Category                | Purpose                                                      |
| ------------------------ | ----------------------- | ------------------------------------------------------------ |
| `automount_token.rego`   | Security (Runtime)      | SA token auto-mount disabled; no default SA usage            |
| `configmap_hygiene.rego` | ConfigMaps/Secrets      | Credential-like keys in ConfigMaps; secret mount read-only   |
| `default_namespace.rego` | Namespace Hygiene       | Resources should not use `default` namespace                 |
| `high_uid.rego`          | Security Context (SCC)  | runAsUser ≥ 10000; no root; runAsNonRoot set                 |
| `image_pull_policy.rego` | Security (Supply Chain) | No :latest; imagePullPolicy=Always for mutable tags          |
| `image_registry.rego`    | Security (Supply Chain) | Images from approved registries only; digest pinning         |
| `labels.rego`            | Labels (Application)    | 6 standard app.kubernetes.io/\* labels on workloads          |
| `network_policy.rego`    | Networking              | At least one NetworkPolicy exists per deployment set         |
| `pdb.rego`               | Pod Disruption Budget   | HA workloads have PDB; no deadlock configurations            |
| `probes.rego`            | Reliability/HA          | readinessProbe (FAIL), livenessProbe (WARN), startupProbe    |
| `rbac.rego`              | RBAC                    | No wildcard verbs/resources; no escalate/bind grants         |
| `reliability.rego`       | Reliability/HA          | Multiple replicas; RollingUpdate strategy                    |
| `resource_limits.rego`   | Configuration           | CPU/memory requests (FAIL) and limits (WARN)                 |
| `seccomp.rego`           | Security Context (SCC)  | seccompProfile RuntimeDefault/Localhost; no Unconfined       |
| `security_context.rego`  | Security Context (SCC)  | No privilege escalation, caps, hostNetwork/PID/IPC, hostPath |
| `storage.rego`           | Storage                 | PVC accessModes and storage size declared                    |
