# Resource Limits & Requests Policy
#
# Ensures all containers in workload resources have:
#   - CPU and memory requests (FAIL - scheduling requires these)
#   - CPU and memory limits  (WARN - best practice for stability)

package artigen.policies.resource_limits

import rego.v1

# - Category metadata --------------
_category := "Configuration"
_reference := "https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/#resource-requests-and-limits-of-pod-and-container"

_workload_kinds := {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}

# Collect all containers from pod spec (both containers and initContainers)
_pod_containers(manifest) := array.concat(
	object.get(manifest, ["spec", "template", "spec", "containers"], []),
	object.get(manifest, ["spec", "template", "spec", "initContainers"], []),
)

# - Missing requests (FAIL) -------------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds

	some container in _pod_containers(manifest)
	requests := object.get(container, ["resources", "requests"], {})
	not requests.cpu

	violation := {
		"msg": sprintf("Container '%s' in %s/%s has no CPU request", [container.name, manifest.kind, manifest.metadata.name]),
		"severity": "fail",
		"category": _category,
		"objective": "Every container must declare CPU and memory requests",
		"expected_outcome": "resources.requests.cpu set on every container",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Set resources.requests.cpu on container '%s'", [container.name]),
		"reference": _reference,
	}
}

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds

	some container in _pod_containers(manifest)
	requests := object.get(container, ["resources", "requests"], {})
	not requests.memory

	violation := {
		"msg": sprintf("Container '%s' in %s/%s has no memory request", [container.name, manifest.kind, manifest.metadata.name]),
		"severity": "fail",
		"category": _category,
		"objective": "Every container must declare CPU and memory requests",
		"expected_outcome": "resources.requests.memory set on every container",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Set resources.requests.memory on container '%s'", [container.name]),
		"reference": _reference,
	}
}

# - Missing limits (WARN) --------------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds

	some container in _pod_containers(manifest)
	limits := object.get(container, ["resources", "limits"], {})
	not limits.cpu

	violation := {
		"msg": sprintf("Container '%s' in %s/%s has no CPU limit", [container.name, manifest.kind, manifest.metadata.name]),
		"severity": "warn",
		"category": _category,
		"objective": "Set CPU and memory limits to prevent resource starvation",
		"expected_outcome": "resources.limits.cpu set on every container",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Set resources.limits.cpu on container '%s'", [container.name]),
		"reference": _reference,
	}
}

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds

	some container in _pod_containers(manifest)
	limits := object.get(container, ["resources", "limits"], {})
	not limits.memory

	violation := {
		"msg": sprintf("Container '%s' in %s/%s has no memory limit", [container.name, manifest.kind, manifest.metadata.name]),
		"severity": "warn",
		"category": _category,
		"objective": "Set CPU and memory limits to prevent resource starvation",
		"expected_outcome": "resources.limits.memory set on every container",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Set resources.limits.memory on container '%s'", [container.name]),
		"reference": _reference,
	}
}
