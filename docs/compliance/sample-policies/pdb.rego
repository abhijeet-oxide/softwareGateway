# Pod Disruption Budget Policy
#
# Checks:
#   - Multi-replica Deployments/StatefulSets should have a PDB (WARN)
#   - PDB minAvailable + maxUnavailable must not cause deadlocks (FAIL)
#   - Single-replica workloads should not have a PDB (INFO)

package artigen.policies.pdb

import rego.v1

# - Category metadata  --------------
_category := "Pod Disruption Budget (PDB)"
_reference := "https://kubernetes.io/docs/tasks/run-application/configure-pdb/"

_ha_kinds := {"Deployment", "StatefulSet"}

# Collect names of workloads with replicas > 1
_ha_workloads contains name if {
	some manifest in input.manifests
	manifest.kind in _ha_kinds
	replicas := object.get(manifest, ["spec", "replicas"], 1)
	replicas > 1
	name := manifest.metadata.name
}

# Collect names of single-replica workloads
_single_replica_workloads contains name if {
	some manifest in input.manifests
	manifest.kind in _ha_kinds
	replicas := object.get(manifest, ["spec", "replicas"], 1)
	replicas <= 1
	name := manifest.metadata.name
}

# Collect workload names that have a PDB targeting them
_pdb_targets contains target_name if {
	some manifest in input.manifests
	manifest.kind == "PodDisruptionBudget"
	selector := object.get(manifest, ["spec", "selector", "matchLabels"], {})
	some key, val in selector
	# PDB typically targets via app label - collect any selector value
	target_name := val
}

# - HA workload without PDB -------------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _ha_kinds
	replicas := object.get(manifest, ["spec", "replicas"], 1)
	replicas > 1
	name := manifest.metadata.name

	# Check if any PDB targets this workload name via matchLabels values
	not _has_pdb_for(name, manifest)

	violation := {
		"msg": sprintf("%s/%s has %d replicas but no PodDisruptionBudget found", [manifest.kind, name, replicas]),
		"severity": "warn",
		"category": _category,
		"objective": "Every HA workload (replicas > 1) should have a PDB",
		"expected_outcome": "PodDisruptionBudget exists with matching selector",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": name,
		},
		"remediation": sprintf("Create a PodDisruptionBudget targeting %s/%s", [manifest.kind, name]),
		"reference": _reference,
	}
}

# Helper - check if any PDB matchLabels match workload's template labels
_has_pdb_for(wl_name, wl_manifest) if {
	wl_labels := object.get(wl_manifest, ["spec", "template", "metadata", "labels"], {})
	some pdb in input.manifests
	pdb.kind == "PodDisruptionBudget"
	pdb_sel := object.get(pdb, ["spec", "selector", "matchLabels"], {})
	count(pdb_sel) > 0
	_labels_subset(pdb_sel, wl_labels)
}

# Check that all key-value pairs in subset exist in superset
_labels_subset(subset, superset) if {
	every key, val in subset {
		superset[key] == val
	}
}

# - PDB maxUnavailable == 0 (deadlock) -------------â”€
violations contains violation if {
	some manifest in input.manifests
	manifest.kind == "PodDisruptionBudget"
	max_unav := object.get(manifest, ["spec", "maxUnavailable"], "")
	max_unav == 0

	violation := {
		"msg": sprintf("PDB/%s: maxUnavailable is 0 - blocks all voluntary evictions", [manifest.metadata.name]),
		"severity": "fail",
		"category": _category,
		"objective": "PDB must not block all voluntary evictions (deadlock)",
		"expected_outcome": "maxUnavailable > 0 or minAvailable < replicas",
		"resource": {
			"kind": "PodDisruptionBudget",
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": "Set maxUnavailable to at least 1 or use minAvailable instead",
		"reference": _reference,
	}
}

# - Single-replica workload with PDB (INFO) -----------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _ha_kinds
	replicas := object.get(manifest, ["spec", "replicas"], 1)
	replicas <= 1
	name := manifest.metadata.name
	_has_pdb_for(name, manifest)

	violation := {
		"msg": sprintf("%s/%s has only %d replica but a PDB targets it", [manifest.kind, name, replicas]),
		"severity": "info",
		"category": _category,
		"objective": "PDBs are unnecessary for single-replica workloads",
		"expected_outcome": "PDB targeting only multi-replica workloads",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": name,
		},
		"remediation": "Remove the PDB or increase replicas to > 1 for HA",
		"reference": _reference,
	}
}
