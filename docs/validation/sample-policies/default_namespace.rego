# Default Namespace Policy
#
# Checks:
#   - Workload resources should not be deployed to the 'default' namespace (WARN)
#   - ConfigMaps/Secrets/Services in 'default' namespace (INFO - often inherited)
#
# Rationale: The default namespace lacks resource quotas, network policies,
# and RBAC boundaries. Production workloads must use explicit namespaces.

package artigen.policies.default_namespace

import rego.v1

_category := "Namespace Hygiene"
_reference := "CIS Benchmark 5.7.4 - The default namespace should not be used"

_workload_kinds := {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}

_supporting_kinds := {"ConfigMap", "Secret", "Service", "ServiceAccount", "PersistentVolumeClaim"}

# Resources that are inherently cluster-scoped - never flag these
_cluster_scoped := {"Namespace", "ClusterRole", "ClusterRoleBinding", "StorageClass", "PersistentVolume", "CustomResourceDefinition"}

_effective_namespace(manifest) := ns if {
	ns := manifest.metadata.namespace
} else := ""

# ── Workloads in default namespace (WARN) ─────────────────────────
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ns := _effective_namespace(manifest)
	ns == "default"

	violation := {
		"msg": sprintf("%s/%s is deployed to the 'default' namespace", [manifest.kind, manifest.metadata.name]),
		"severity": "warn",
		"category": _category,
		"objective": "Workloads should use dedicated namespaces (not 'default')",
		"expected_outcome": "metadata.namespace is set to a dedicated namespace",
		"resource": {
			"kind": manifest.kind,
			"namespace": "default",
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Move %s/%s to a dedicated namespace with appropriate RBAC and NetworkPolicies", [manifest.kind, manifest.metadata.name]),
		"reference": _reference,
	}
}

# ── Supporting resources in default namespace (INFO) ──────────────
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _supporting_kinds
	not manifest.kind in _cluster_scoped
	ns := _effective_namespace(manifest)
	ns == "default"

	violation := {
		"msg": sprintf("%s/%s is in the 'default' namespace", [manifest.kind, manifest.metadata.name]),
		"severity": "info",
		"category": _category,
		"objective": "All resources should use dedicated namespaces",
		"expected_outcome": "metadata.namespace is set to a non-default namespace",
		"resource": {
			"kind": manifest.kind,
			"namespace": "default",
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Set metadata.namespace on %s/%s to the target deployment namespace", [manifest.kind, manifest.metadata.name]),
		"reference": _reference,
	}
}
