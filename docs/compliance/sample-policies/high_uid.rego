# High UID Policy
#
# Checks:
#   - Containers should run as a high UID (>= 10000) to avoid host conflict (WARN)
#   - runAsUser == 0 (root) is a security failure (FAIL)
#   - runAsNonRoot should be true at pod or container level (WARN)
#
# Rationale: UIDs < 10000 may collide with host system users, enabling
# privilege escalation via shared filesystems or /proc.

package artigen.policies.high_uid

import rego.v1

_category := "Security Context Constraints (SCC)"
_reference := "CIS Benchmark 5.2.6 - Minimize the admission of root containers"

_workload_kinds := {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}

_pod_spec(manifest) := manifest.spec.template.spec if {
	manifest.kind in _workload_kinds
}

_all_containers(manifest) := array.concat(
	object.get(_pod_spec(manifest), "containers", []),
	object.get(_pod_spec(manifest), "initContainers", []),
)

_workload_label(manifest) := sprintf("%s/%s", [manifest.kind, manifest.metadata.name])

# Effective runAsUser: container-level overrides pod-level
_effective_uid(manifest, container) := uid if {
	uid := container.securityContext.runAsUser
} else := uid if {
	ps := _pod_spec(manifest)
	uid := ps.securityContext.runAsUser
}

# - runAsUser == 0 (root) â†’ FAIL ----------------â”€
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	some container in _all_containers(manifest)
	uid := _effective_uid(manifest, container)
	uid == 0

	violation := {
		"msg": sprintf("Container '%s' in %s runs as root (UID 0)", [container.name, _workload_label(manifest)]),
		"severity": "fail",
		"category": _category,
		"objective": "Containers must not run as root",
		"expected_outcome": "runAsUser > 0 or runAsNonRoot: true",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Set securityContext.runAsUser to a non-zero UID on container '%s', or set runAsNonRoot: true", [container.name]),
		"reference": _reference,
	}
}

# - runAsUser < 10000 (low UID, host conflict risk) â†’ WARN ---â”€
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	some container in _all_containers(manifest)
	uid := _effective_uid(manifest, container)
	uid > 0
	uid < 10000

	violation := {
		"msg": sprintf("Container '%s' in %s uses low UID %d (< 10000) - risk of host UID collision", [container.name, _workload_label(manifest), uid]),
		"severity": "warn",
		"category": _category,
		"objective": "Containers should run as a high UID to avoid host conflict",
		"expected_outcome": "runAsUser >= 10000",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Set securityContext.runAsUser >= 10000 on container '%s'", [container.name]),
		"reference": _reference,
	}
}

# - runAsNonRoot not set anywhere â†’ WARN -------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)

	# Pod-level runAsNonRoot not set or false
	pod_rnr := object.get(ps, ["securityContext", "runAsNonRoot"], false)
	pod_rnr != true

	some container in object.get(ps, "containers", [])
	# Container-level runAsNonRoot not set or false
	ctr_rnr := object.get(container, ["securityContext", "runAsNonRoot"], false)
	ctr_rnr != true

	# Also no explicit runAsUser set
	not container.securityContext.runAsUser
	not ps.securityContext.runAsUser

	violation := {
		"msg": sprintf("Container '%s' in %s has no runAsNonRoot or explicit runAsUser - may run as root", [container.name, _workload_label(manifest)]),
		"severity": "warn",
		"category": _category,
		"objective": "Explicitly prevent root execution via runAsNonRoot: true",
		"expected_outcome": "runAsNonRoot is true at pod or container level",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Set securityContext.runAsNonRoot: true on pod spec or container '%s'", [container.name]),
		"reference": _reference,
	}
}
