# Seccomp Profile Policy
#
# Checks:
#   - Pod or container seccompProfile should be RuntimeDefault or Localhost (WARN)
#   - seccompProfile type 'Unconfined' is explicitly insecure (FAIL)
#
# Rationale: seccomp restricts syscalls available to containers. Without it,
# containers can invoke any syscall the kernel supports - including those
# used in container escapes.
#
# Since Kubernetes 1.27, RuntimeDefault is the recommended baseline.

package artigen.policies.seccomp

import rego.v1

_category := "Security Context Constraints (SCC)"
_reference := "https://kubernetes.io/docs/tutorials/security/seccomp/"

_workload_kinds := {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}

_pod_spec(manifest) := manifest.spec.template.spec if {
	manifest.kind in _workload_kinds
}

_workload_label(manifest) := sprintf("%s/%s", [manifest.kind, manifest.metadata.name])

_acceptable_profiles := {"RuntimeDefault", "Localhost"}

# Pod-level seccompProfile type
_pod_seccomp_type(manifest) := stype if {
	ps := _pod_spec(manifest)
	stype := ps.securityContext.seccompProfile.type
}

# Container-level seccompProfile type
_container_seccomp_type(container) := stype if {
	stype := container.securityContext.seccompProfile.type
}

# - Explicitly Unconfined â†’ FAIL ----------------â”€
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	pod_type := _pod_seccomp_type(manifest)
	pod_type == "Unconfined"

	violation := {
		"msg": sprintf("%s: pod-level seccompProfile is 'Unconfined' - all syscalls permitted", [_workload_label(manifest)]),
		"severity": "fail",
		"category": _category,
		"objective": "Containers must not use 'Unconfined' seccomp profile",
		"expected_outcome": "seccompProfile.type is RuntimeDefault or Localhost",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": "Set spec.securityContext.seccompProfile.type to 'RuntimeDefault'",
		"reference": _reference,
	}
}

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	some container in object.get(ps, "containers", [])
	ctr_type := _container_seccomp_type(container)
	ctr_type == "Unconfined"

	violation := {
		"msg": sprintf("Container '%s' in %s has seccompProfile 'Unconfined'", [container.name, _workload_label(manifest)]),
		"severity": "fail",
		"category": _category,
		"objective": "Containers must not use 'Unconfined' seccomp profile",
		"expected_outcome": "seccompProfile.type is RuntimeDefault or Localhost",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Set securityContext.seccompProfile.type to 'RuntimeDefault' on container '%s'", [container.name]),
		"reference": _reference,
	}
}

# - No seccompProfile set at all â†’ WARN -------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)

	# No pod-level seccomp
	not ps.securityContext.seccompProfile

	some container in object.get(ps, "containers", [])
	# No container-level seccomp
	not container.securityContext.seccompProfile

	violation := {
		"msg": sprintf("Container '%s' in %s has no seccompProfile set - defaults to Unconfined on most clusters", [container.name, _workload_label(manifest)]),
		"severity": "warn",
		"category": _category,
		"objective": "Seccomp profile should be set to RuntimeDefault or Localhost",
		"expected_outcome": "seccompProfile.type is RuntimeDefault or Localhost at pod or container level",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Add securityContext.seccompProfile: {type: RuntimeDefault} to pod spec or container '%s'", [container.name]),
		"reference": _reference,
	}
}
