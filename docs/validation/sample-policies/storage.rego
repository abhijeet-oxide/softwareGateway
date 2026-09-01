# Storage Policy
#
# Checks:
#   - PVC should declare explicit accessModes (WARN)
#   - PVC should have a storage size request (WARN)
#   - RWX (ReadWriteMany) on single-pod workloads is likely unnecessary (INFO)
#   - StatefulSet volumeClaimTemplates should have storage class (WARN)

package artigen.policies.storage

import rego.v1

# - Category metadata --------------
_category := "Storage"
_reference := "https://kubernetes.io/docs/concepts/storage/persistent-volumes/"

# - PVC missing accessModes (WARN) ---------------â”€
violations contains violation if {
	some manifest in input.manifests
	manifest.kind == "PersistentVolumeClaim"
	access_modes := object.get(manifest, ["spec", "accessModes"], [])
	count(access_modes) == 0

	violation := {
		"msg": sprintf("PVC/%s: no accessModes specified", [manifest.metadata.name]),
		"severity": "warn",
		"category": _category,
		"objective": "PVCs must declare explicit accessModes",
		"expected_outcome": "spec.accessModes contains at least one mode",
		"resource": {
			"kind": "PersistentVolumeClaim",
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": "Add spec.accessModes (e.g. ['ReadWriteOnce']) to the PVC",
		"reference": _reference,
	}
}

# - PVC missing storage request (WARN) -------------â”€
violations contains violation if {
	some manifest in input.manifests
	manifest.kind == "PersistentVolumeClaim"
	resources := object.get(manifest, ["spec", "resources", "requests"], {})
	not resources.storage

	violation := {
		"msg": sprintf("PVC/%s: no storage size request", [manifest.metadata.name]),
		"severity": "warn",
		"category": _category,
		"objective": "PVCs must request a specific storage size",
		"expected_outcome": "spec.resources.requests.storage is set",
		"resource": {
			"kind": "PersistentVolumeClaim",
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": "Add spec.resources.requests.storage (e.g. '10Gi') to the PVC",
		"reference": _reference,
	}
}

# - RWX on single-replica workload (INFO) ------------
# Detects PVCs with ReadWriteMany that are mounted by workloads with 1 replica
_single_replica_names contains name if {
	some manifest in input.manifests
	manifest.kind in {"Deployment", "StatefulSet"}
	replicas := object.get(manifest, ["spec", "replicas"], 1)
	replicas <= 1
	name := manifest.metadata.name
}

violations contains violation if {
	some manifest in input.manifests
	manifest.kind == "PersistentVolumeClaim"
	access_modes := object.get(manifest, ["spec", "accessModes"], [])
	some mode in access_modes
	mode == "ReadWriteMany"

	violation := {
		"msg": sprintf("PVC/%s: uses ReadWriteMany - verify multiple pods need concurrent write access", [manifest.metadata.name]),
		"severity": "info",
		"category": _category,
		"objective": "Use ReadWriteOnce unless multiple pods need concurrent writes",
		"expected_outcome": "RWX used only when multiple pods share the volume",
		"resource": {
			"kind": "PersistentVolumeClaim",
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("If only one pod uses PVC/%s, switch to ReadWriteOnce for better performance", [manifest.metadata.name]),
		"reference": _reference,
	}
}

# - StatefulSet volumeClaimTemplates missing storageClassName --â”€
violations contains violation if {
	some manifest in input.manifests
	manifest.kind == "StatefulSet"
	vct := object.get(manifest, ["spec", "volumeClaimTemplates"], [])
	some claim in vct
	sc := object.get(claim, ["spec", "storageClassName"], "")
	sc == ""

	violation := {
		"msg": sprintf("StatefulSet/%s: volumeClaimTemplate '%s' has no storageClassName", [manifest.metadata.name, object.get(claim, ["metadata", "name"], "unnamed")]),
		"severity": "warn",
		"category": _category,
		"objective": "Explicitly set storageClassName in volumeClaimTemplates",
		"expected_outcome": "Each volumeClaimTemplate has a storageClassName",
		"resource": {
			"kind": "StatefulSet",
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Add spec.storageClassName to volumeClaimTemplate in StatefulSet/%s", [manifest.metadata.name]),
		"reference": _reference,
	}
}
