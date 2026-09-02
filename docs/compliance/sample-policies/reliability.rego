# Reliability / High Availability Policy
#
# Checks:
#   - Deployments/StatefulSets should have replicas > 1 (WARN)
#   - Deployment strategy type should be RollingUpdate (WARN)
#   - RollingUpdate maxUnavailable should not exceed 50% (WARN)

package artigen.policies.reliability

import rego.v1

# - Category metadata  --------------
_category := "Reliability/HA"
_reference := "https://kubernetes.io/docs/concepts/workloads/controllers/deployment/"

_ha_kinds := {"Deployment", "StatefulSet"}

# - Single replica (WARN) --------------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _ha_kinds
	replicas := object.get(manifest, ["spec", "replicas"], 1)
	replicas <= 1

	violation := {
		"msg": sprintf("%s/%s has only %d replica - no HA", [manifest.kind, manifest.metadata.name, replicas]),
		"severity": "warn",
		"category": _category,
		"objective": "Production workloads should have multiple replicas for HA",
		"expected_outcome": "replicas >= 2",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Increase spec.replicas to at least 2 on %s/%s", [manifest.kind, manifest.metadata.name]),
		"reference": _reference,
	}
}

# - Deployment strategy not RollingUpdate ------------â”€
violations contains violation if {
	some manifest in input.manifests
	manifest.kind == "Deployment"
	strategy_type := object.get(manifest, ["spec", "strategy", "type"], "RollingUpdate")
	strategy_type != "RollingUpdate"

	violation := {
		"msg": sprintf("Deployment/%s: strategy type is '%s' (prefer RollingUpdate)", [manifest.metadata.name, strategy_type]),
		"severity": "warn",
		"category": _category,
		"objective": "Use RollingUpdate strategy for zero-downtime deployments",
		"expected_outcome": "spec.strategy.type is RollingUpdate",
		"resource": {
			"kind": "Deployment",
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": "Set spec.strategy.type to RollingUpdate",
		"reference": _reference,
	}
}

# - RollingUpdate maxUnavailable too high ------------â”€
# Only fires when maxUnavailable is a raw integer > 1 (percentage
# strings like "25%" can't be reliably parsed without to_number/regex)
violations contains violation if {
	some manifest in input.manifests
	manifest.kind == "Deployment"
	strategy_type := object.get(manifest, ["spec", "strategy", "type"], "RollingUpdate")
	strategy_type == "RollingUpdate"
	max_unav := object.get(manifest, ["spec", "strategy", "rollingUpdate", "maxUnavailable"], 1)
	# Only check when the value is an integer (not a percentage string)
	max_unav > 1

	violation := {
		"msg": sprintf("Deployment/%s: rollingUpdate.maxUnavailable is %v (high)", [manifest.metadata.name, max_unav]),
		"severity": "warn",
		"category": _category,
		"objective": "Limit maxUnavailable during rolling updates",
		"expected_outcome": "maxUnavailable <= 1 or a small percentage",
		"resource": {
			"kind": "Deployment",
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Set spec.strategy.rollingUpdate.maxUnavailable to 1 or '25%%' on Deployment/%s", [manifest.metadata.name]),
		"reference": _reference,
	}
}
