# RBAC Policy
#
# Checks:
#   - No wildcard verbs or resources in Roles/ClusterRoles (FAIL)
#   - ClusterRoleBindings should be audited (WARN)
#   - Secrets access should be minimized (WARN)
#   - pods/exec verb should be minimized (WARN)
#   - escalate / bind verbs should not be granted (FAIL)

package artigen.policies.rbac

import rego.v1

# - Category metadata  --------------
_category := "RBAC"
_reference := "https://kubernetes.io/docs/concepts/security/rbac-good-practices/"

_role_kinds := {"Role", "ClusterRole"}

# - Wildcard resources (FAIL) ------------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _role_kinds
	some rule in manifest.rules
	some res in rule.resources
	res == "*"

	violation := {
		"msg": sprintf("%s/%s: rule grants access to wildcard resources '*'", [manifest.kind, manifest.metadata.name]),
		"severity": "fail",
		"category": _category,
		"objective": "No wildcard resource grants in RBAC roles",
		"expected_outcome": "Resources are explicitly listed (no '*')",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": "Replace wildcard '*' with explicit resource names in the Role/ClusterRole rules",
		"reference": _reference,
	}
}

# - Wildcard verbs (FAIL) --------------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _role_kinds
	some rule in manifest.rules
	some verb in rule.verbs
	verb == "*"

	violation := {
		"msg": sprintf("%s/%s: rule grants wildcard verb '*'", [manifest.kind, manifest.metadata.name]),
		"severity": "fail",
		"category": _category,
		"objective": "No wildcard verb grants in RBAC roles",
		"expected_outcome": "Verbs are explicitly listed (no '*')",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": "Replace wildcard '*' verb with specific verbs (get, list, watch, create, update, delete)",
		"reference": _reference,
	}
}

# - Escalation verbs: escalate, bind (FAIL) -----------
_dangerous_verbs := ["escalate", "bind"]

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _role_kinds
	some rule in manifest.rules
	some verb in rule.verbs
	some dv in _dangerous_verbs
	verb == dv

	violation := {
		"msg": sprintf("%s/%s: rule grants dangerous verb '%s'", [manifest.kind, manifest.metadata.name, verb]),
		"severity": "fail",
		"category": _category,
		"objective": "Prevent privilege escalation via RBAC",
		"expected_outcome": "No 'escalate' or 'bind' verbs in rules",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Remove verb '%s' from %s/%s - it allows privilege escalation", [verb, manifest.kind, manifest.metadata.name]),
		"reference": _reference,
	}
}

# - ClusterRoleBinding audit (WARN) ---------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind == "ClusterRoleBinding"

	violation := {
		"msg": sprintf("ClusterRoleBinding/%s: cluster-wide binding to '%s'", [manifest.metadata.name, object.get(manifest, ["roleRef", "name"], "unknown")]),
		"severity": "warn",
		"category": _category,
		"objective": "Audit all ClusterRoleBindings (prefer namespaced RoleBindings)",
		"expected_outcome": "Only essential ClusterRoleBindings exist",
		"resource": {
			"kind": "ClusterRoleBinding",
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": "Consider using a namespaced RoleBinding instead if cluster-wide access is not needed",
		"reference": _reference,
	}
}

# - Secrets access (WARN) --------------------
_secret_write_verbs := ["create", "update", "patch", "delete"]

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _role_kinds
	some rule in manifest.rules
	some res in rule.resources
	res == "secrets"
	some verb in rule.verbs
	some wv in _secret_write_verbs
	verb == wv

	violation := {
		"msg": sprintf("%s/%s: grants '%s' on secrets", [manifest.kind, manifest.metadata.name, verb]),
		"severity": "warn",
		"category": _category,
		"objective": "Minimize write access to secrets",
		"expected_outcome": "Only read (get/list/watch) access to secrets unless justified",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Review if '%s' on secrets is needed in %s/%s - prefer read-only access", [verb, manifest.kind, manifest.metadata.name]),
		"reference": _reference,
	}
}

# - pods/exec access (WARN) -------------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _role_kinds
	some rule in manifest.rules
	some res in rule.resources
	res == "pods/exec"

	violation := {
		"msg": sprintf("%s/%s: grants access to pods/exec", [manifest.kind, manifest.metadata.name]),
		"severity": "warn",
		"category": _category,
		"objective": "Minimize pods/exec access (remote code execution risk)",
		"expected_outcome": "pods/exec not granted unless justified",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Review if pods/exec access is required in %s/%s", [manifest.kind, manifest.metadata.name]),
		"reference": _reference,
	}
}
