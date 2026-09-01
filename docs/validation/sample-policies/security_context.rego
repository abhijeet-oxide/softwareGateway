# Security Context Policy
#
# Checks Kubernetes pod and container securityContext settings across
# all workload manifests. Covers:
#   - allowPrivilegeEscalation must be false
#   - privileged must not be true (FAIL)
#   - runAsNonRoot should be true or runAsUser > 0
#   - runAsUser should not be 0
#   - readOnlyRootFilesystem should be true
#   - capabilities.drop should include ALL
#   - capabilities.add should be empty
#   - seccompProfile should be RuntimeDefault or Localhost
#   - procMount should not be Unmasked
#   - hostNetwork, hostPID, hostIPC should not be enabled (FAIL)
#   - hostPath volumes should not be used (FAIL)
#   - pod-level fsGroup should be set to non-root value

package artigen.policies.security_context

import rego.v1

# ── Category metadata  ────────────────────────────
_category := "Security Context Constraints (SCC)"
_reference := "https://kubernetes.io/docs/concepts/security/pod-security-standards/"

_workload_kinds := {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"}

_pod_spec(manifest) := manifest.spec.template.spec if {
	manifest.kind in {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}
}

_pod_spec(manifest) := manifest.spec if {
	manifest.kind == "Pod"
}

_workload_label(manifest) := sprintf("%s/%s", [manifest.kind, manifest.metadata.name])

# ── Pod-level: hostNetwork (FAIL) ─────────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	ps.hostNetwork == true
	violation := {
		"msg": sprintf("%s: hostNetwork is enabled", [_workload_label(manifest)]),
		"severity": "fail",
		"category": _category,
		"objective": "Prevent pods from using the host network namespace",
		"expected_outcome": "hostNetwork is false or unset",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": "Set spec.hostNetwork to false unless host networking is required",
		"reference": _reference,
	}
}

# ── Pod-level: hostPID (FAIL) ─────────────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	ps.hostPID == true
	violation := {
		"msg": sprintf("%s: hostPID is enabled", [_workload_label(manifest)]),
		"severity": "fail",
		"category": _category,
		"objective": "Prevent pods from sharing the host PID namespace",
		"expected_outcome": "hostPID is false or unset",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": "Set spec.hostPID to false unless host PID namespace access is required",
		"reference": _reference,
	}
}

# ── Pod-level: hostIPC (FAIL) ─────────────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	ps.hostIPC == true
	violation := {
		"msg": sprintf("%s: hostIPC is enabled", [_workload_label(manifest)]),
		"severity": "fail",
		"category": _category,
		"objective": "Prevent pods from sharing the host IPC namespace",
		"expected_outcome": "hostIPC is false or unset",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": "Set spec.hostIPC to false unless host IPC namespace access is required",
		"reference": _reference,
	}
}

# ── Pod-level: hostPath volumes (FAIL) ────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	volumes := object.get(ps, "volumes", [])
	some vol in volumes
	object.get(vol, "hostPath", false) != false
	hp := vol.hostPath
	violation := {
		"msg": sprintf("%s: volume '%s' uses hostPath '%s'", [_workload_label(manifest), vol.name, object.get(hp, "path", "")]),
		"severity": "fail",
		"category": _category,
		"objective": "Prevent hostPath volume mounts (node filesystem exposure)",
		"expected_outcome": "No volumes with hostPath type",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": sprintf("Replace hostPath volume '%s' with a PVC, ConfigMap, or emptyDir", [vol.name]),
		"reference": _reference,
	}
}

# ── Pod-level: fsGroup not set ────────────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	sc := object.get(ps, "securityContext", {})
	not sc.fsGroup
	violation := {
		"msg": sprintf("%s: pod fsGroup is not set", [_workload_label(manifest)]),
		"severity": "info",
		"category": _category,
		"objective": "Set fsGroup for proper volume ownership",
		"expected_outcome": "securityContext.fsGroup set to a non-root GID",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": "Set securityContext.fsGroup to a non-root GID for volume ownership",
		"reference": _reference,
	}
}

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	sc := object.get(ps, "securityContext", {})
	sc.fsGroup == 0
	violation := {
		"msg": sprintf("%s: pod fsGroup is 0 (root)", [_workload_label(manifest)]),
		"severity": "warn",
		"category": _category,
		"objective": "Set fsGroup for proper volume ownership",
		"expected_outcome": "securityContext.fsGroup set to a non-root GID",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": "Set securityContext.fsGroup to a non-root value (e.g. 1000)",
		"reference": _reference,
	}
}

# ── Pod-level: seccompProfile ─────────────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	sc := object.get(ps, "securityContext", {})
	profile := object.get(sc, "seccompProfile", {})
	profile_type := object.get(profile, "type", "")
	profile_type != ""
	not profile_type in {"RuntimeDefault", "Localhost"}
	violation := {
		"msg": sprintf("%s: pod seccompProfile type is '%s'", [_workload_label(manifest), profile_type]),
		"severity": "warn",
		"category": _category,
		"objective": "Use RuntimeDefault or Localhost seccomp profile",
		"expected_outcome": "seccompProfile.type is RuntimeDefault or Localhost",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": "Set securityContext.seccompProfile.type to RuntimeDefault or Localhost",
		"reference": _reference,
	}
}

# ── Container-level checks ────────────────────────────────────────

_all_containers(ps) := array.concat(
	array.concat(
		object.get(ps, "containers", []),
		object.get(ps, "initContainers", []),
	),
	object.get(ps, "ephemeralContainers", []),
)

# ── privileged (FAIL) ─────────────────────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	some container in _all_containers(ps)
	sc := object.get(container, "securityContext", {})
	sc.privileged == true
	violation := {
		"msg": sprintf("%s container '%s': privileged is true", [_workload_label(manifest), container.name]),
		"severity": "fail",
		"category": _category,
		"objective": "Containers must not run in privileged mode",
		"expected_outcome": "securityContext.privileged is false or unset",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": sprintf("Set securityContext.privileged to false on container '%s'", [container.name]),
		"reference": _reference,
	}
}

# ── allowPrivilegeEscalation ──────────────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	some container in _all_containers(ps)
	sc := object.get(container, "securityContext", {})
	not sc.allowPrivilegeEscalation == false
	violation := {
		"msg": sprintf("%s container '%s': allowPrivilegeEscalation is not explicitly false", [_workload_label(manifest), container.name]),
		"severity": "warn",
		"category": _category,
		"objective": "Explicitly disable privilege escalation on all containers",
		"expected_outcome": "allowPrivilegeEscalation is false",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": sprintf("Set securityContext.allowPrivilegeEscalation to false on container '%s'", [container.name]),
		"reference": _reference,
	}
}

# ── runAsUser == 0 ────────────────────────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	some container in _all_containers(ps)
	sc := object.get(container, "securityContext", {})
	sc.runAsUser == 0
	violation := {
		"msg": sprintf("%s container '%s': runAsUser is 0 (root)", [_workload_label(manifest), container.name]),
		"severity": "warn",
		"category": _category,
		"objective": "Containers should not run as root (UID 0)",
		"expected_outcome": "runAsUser > 0",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": sprintf("Set securityContext.runAsUser to a non-root UID on container '%s'", [container.name]),
		"reference": _reference,
	}
}

# ── runAsNonRoot not set ──────────────────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	pod_sc := object.get(ps, "securityContext", {})
	some container in _all_containers(ps)
	sc := object.get(container, "securityContext", {})
	not sc.runAsNonRoot == true
	not pod_sc.runAsNonRoot == true
	violation := {
		"msg": sprintf("%s container '%s': runAsNonRoot is not true (pod or container level)", [_workload_label(manifest), container.name]),
		"severity": "warn",
		"category": _category,
		"objective": "Enforce runAsNonRoot at pod or container level",
		"expected_outcome": "runAsNonRoot is true",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": sprintf("Set securityContext.runAsNonRoot to true on container '%s' or pod level", [container.name]),
		"reference": _reference,
	}
}

# ── readOnlyRootFilesystem ────────────────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	some container in _all_containers(ps)
	sc := object.get(container, "securityContext", {})
	not sc.readOnlyRootFilesystem == true
	violation := {
		"msg": sprintf("%s container '%s': readOnlyRootFilesystem is not true", [_workload_label(manifest), container.name]),
		"severity": "info",
		"category": _category,
		"objective": "Use read-only root filesystem to limit attack surface",
		"expected_outcome": "readOnlyRootFilesystem is true",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": sprintf("Set securityContext.readOnlyRootFilesystem to true on container '%s'", [container.name]),
		"reference": _reference,
	}
}

# ── capabilities.drop should include ALL ──────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	some container in _all_containers(ps)
	sc := object.get(container, "securityContext", {})
	caps := object.get(sc, "capabilities", {})
	drop := object.get(caps, "drop", [])
	not "ALL" in drop
	violation := {
		"msg": sprintf("%s container '%s': capabilities.drop does not include ALL", [_workload_label(manifest), container.name]),
		"severity": "warn",
		"category": _category,
		"objective": "Drop all Linux capabilities by default",
		"expected_outcome": "capabilities.drop includes ALL",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": sprintf("Add 'ALL' to securityContext.capabilities.drop on container '%s'", [container.name]),
		"reference": _reference,
	}
}

# ── capabilities.add should be empty ──────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	some container in _all_containers(ps)
	sc := object.get(container, "securityContext", {})
	caps := object.get(sc, "capabilities", {})
	add_caps := object.get(caps, "add", [])
	count(add_caps) > 0
	some cap in add_caps
	violation := {
		"msg": sprintf("%s container '%s': capability '%s' is added", [_workload_label(manifest), container.name, cap]),
		"severity": "info",
		"category": _category,
		"objective": "Minimize added Linux capabilities",
		"expected_outcome": "No capabilities in capabilities.add (or only essential ones)",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": sprintf("Review whether capability '%s' is required on container '%s'", [cap, container.name]),
		"reference": _reference,
	}
}

# ── Container seccompProfile ──────────────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	some container in _all_containers(ps)
	sc := object.get(container, "securityContext", {})
	profile := object.get(sc, "seccompProfile", {})
	profile_type := object.get(profile, "type", "")
	profile_type != ""
	not profile_type in {"RuntimeDefault", "Localhost"}
	violation := {
		"msg": sprintf("%s container '%s': seccompProfile type is '%s'", [_workload_label(manifest), container.name, profile_type]),
		"severity": "warn",
		"category": _category,
		"objective": "Use RuntimeDefault or Localhost seccomp profile",
		"expected_outcome": "seccompProfile.type is RuntimeDefault or Localhost",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": sprintf("Set seccompProfile.type to RuntimeDefault or Localhost on container '%s'", [container.name]),
		"reference": _reference,
	}
}

# ── procMount ─────────────────────────────────────────────────────

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)
	some container in _all_containers(ps)
	sc := object.get(container, "securityContext", {})
	sc.procMount == "Unmasked"
	violation := {
		"msg": sprintf("%s container '%s': procMount is Unmasked", [_workload_label(manifest), container.name]),
		"severity": "warn",
		"category": _category,
		"objective": "Prevent unmasked /proc mount (information disclosure risk)",
		"expected_outcome": "procMount is Default or unset",
		"resource": {"kind": manifest.kind, "name": manifest.metadata.name},
		"remediation": sprintf("Set procMount to Default on container '%s' unless unmasked /proc is required", [container.name]),
		"reference": _reference,
	}
}
