# Health Probes Policy
#
# Checks:
#   - Containers in Deployments/StatefulSets must have readinessProbe (FAIL)
#   - Containers in Deployments/StatefulSets must have livenessProbe (WARN)
#   - Containers in Deployments/StatefulSets should have startupProbe for
#     slow-starting apps (INFO)
#
# Rationale:
#   - readinessProbe: without it, Service endpoints include unready pods â†’ 5xx
#   - livenessProbe: without it, stuck containers are never restarted
#   - startupProbe: prevents liveness from killing slow-booting apps

package artigen.policies.probes

import rego.v1

_category := "Reliability/HA"
_reference := "https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/"

_workload_kinds := {"Deployment", "StatefulSet"}

_pod_spec(manifest) := manifest.spec.template.spec if {
	manifest.kind in _workload_kinds
}

# Only check main containers, not init/ephemeral
_containers(manifest) := object.get(_pod_spec(manifest), "containers", [])

# - Missing readinessProbe (FAIL) ----------------â”€
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	some container in _containers(manifest)
	not container.readinessProbe

	violation := {
		"msg": sprintf("Container '%s' in %s/%s has no readinessProbe", [container.name, manifest.kind, manifest.metadata.name]),
		"severity": "fail",
		"category": _category,
		"objective": "Every long-running container must have a readinessProbe",
		"expected_outcome": "readinessProbe is configured on all containers",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Add readinessProbe (httpGet, tcpSocket, or exec) to container '%s'", [container.name]),
		"reference": _reference,
	}
}

# - Missing livenessProbe (WARN) -----------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	some container in _containers(manifest)
	not container.livenessProbe

	violation := {
		"msg": sprintf("Container '%s' in %s/%s has no livenessProbe", [container.name, manifest.kind, manifest.metadata.name]),
		"severity": "warn",
		"category": _category,
		"objective": "Every long-running container should have a livenessProbe",
		"expected_outcome": "livenessProbe is configured on all containers",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Add livenessProbe to container '%s' - use a higher initialDelaySeconds than readiness to avoid premature restarts", [container.name]),
		"reference": _reference,
	}
}

# - Missing startupProbe when initialDelaySeconds > 30 (INFO) --
# If liveness has a high initialDelaySeconds, a startupProbe is better
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	some container in _containers(manifest)
	not container.startupProbe
	container.livenessProbe
	delay := object.get(container.livenessProbe, "initialDelaySeconds", 0)
	delay > 30

	violation := {
		"msg": sprintf("Container '%s' in %s/%s has livenessProbe.initialDelaySeconds=%d but no startupProbe", [container.name, manifest.kind, manifest.metadata.name, delay]),
		"severity": "info",
		"category": _category,
		"objective": "Slow-starting containers should use startupProbe instead of high initialDelaySeconds",
		"expected_outcome": "startupProbe is configured for containers with boot time > 30s",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Add startupProbe to container '%s' and reduce livenessProbe.initialDelaySeconds - startupProbe gates liveness until the app is booted", [container.name]),
		"reference": _reference,
	}
}

# - Missing startupProbe entirely (INFO - advisory) -------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	some container in _containers(manifest)
	not container.startupProbe
	container.livenessProbe
	delay := object.get(container.livenessProbe, "initialDelaySeconds", 0)
	delay <= 30
	# Only fire if there is no startupProbe and liveness exists
	# This is purely informational for awareness

	violation := {
		"msg": sprintf("Container '%s' in %s/%s has no startupProbe", [container.name, manifest.kind, manifest.metadata.name]),
		"severity": "info",
		"category": _category,
		"objective": "Consider startupProbe for containers that may take variable time to start",
		"expected_outcome": "startupProbe configured for graceful boot detection",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Consider adding startupProbe to container '%s' for more reliable boot detection", [container.name]),
		"reference": _reference,
	}
}
