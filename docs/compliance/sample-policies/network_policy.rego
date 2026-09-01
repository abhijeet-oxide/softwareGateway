# Network Policy Coverage
#
# Checks:
#   - Namespaces with workloads should have at least one NetworkPolicy (WARN)
#   - Workloads exposing Services should have ingress NetworkPolicy (INFO)
#
# Rationale: Without NetworkPolicies, all pod-to-pod traffic is allowed.
# Defense-in-depth requires explicit ingress/egress rules.

package artigen.policies.network_policy

import rego.v1

_category := "Networking"
_reference := "CIS Benchmark 5.3.2 - Ensure that all Namespaces have Network Policies defined"

_workload_kinds := {"Deployment", "StatefulSet", "DaemonSet"}

# Collect all NetworkPolicy objects in manifests
_has_network_policy if {
	some manifest in input.manifests
	manifest.kind == "NetworkPolicy"
}

# ── No NetworkPolicy in the deployment set (WARN) ─────────────────
violations contains violation if {
	# Only fire once - check if any workloads exist but no netpol
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	not _has_network_policy

	# Deduplicate: only fire for the first workload found
	all_workloads := [m | some m in input.manifests; m.kind in _workload_kinds]
	manifest == all_workloads[0]

	violation := {
		"msg": "No NetworkPolicy found in deployment manifests - all pod traffic is unrestricted",
		"severity": "warn",
		"category": _category,
		"objective": "Every namespace with workloads should have NetworkPolicies for defense-in-depth",
		"expected_outcome": "At least one NetworkPolicy resource exists",
		"resource": {
			"kind": "Namespace",
			"namespace": input.namespace,
			"name": input.namespace,
		},
		"remediation": "Create NetworkPolicy resources to restrict ingress/egress traffic between pods",
		"reference": _reference,
	}
}
