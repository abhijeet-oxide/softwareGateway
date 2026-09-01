# Service Account Token Policy
#
# Checks:
#   - automountServiceAccountToken should be false (WARN)
#   - Pods should not use the default service account (FAIL)

package artigen.policies.automount_token

import rego.v1

# - Category metadata  --------------
_category := "Security (Runtime & Supply Chain)"
_reference := "CIS Benchmark 5.1.6 - Ensure that Service Account Tokens are only mounted where necessary"

_workload_kinds := {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}

_pod_spec(manifest) := manifest.spec.template.spec if {
	manifest.kind in _workload_kinds
}

# - automountServiceAccountToken not false ------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	amt := object.get(ps, "automountServiceAccountToken", true)
	amt != false

	violation := {
		"msg": sprintf("%s/%s: automountServiceAccountToken is not false", [manifest.kind, manifest.metadata.name]),
		"severity": "warn",
		"category": _category,
		"objective": "Disable automatic mounting of service account tokens unless needed",
		"expected_outcome": "automountServiceAccountToken is false",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": "Set spec.template.spec.automountServiceAccountToken to false unless the pod needs Kubernetes API access",
		"reference": _reference,
	}
}

# - Using default service account ----------------â”€
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	sa := object.get(ps, "serviceAccountName", "default")
	sa == "default"

	violation := {
		"msg": sprintf("%s/%s: uses the 'default' service account", [manifest.kind, manifest.metadata.name]),
		"severity": "fail",
		"category": _category,
		"objective": "Use dedicated service accounts (never the default SA)",
		"expected_outcome": "serviceAccountName set to a dedicated (non-default) service account",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": "Create a dedicated ServiceAccount and set spec.template.spec.serviceAccountName",
		"reference": _reference,
	}
}
