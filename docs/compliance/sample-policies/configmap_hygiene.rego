# ConfigMap & Secret Hygiene Policy
#
# Checks:
#   - ConfigMaps should not contain credential-like keys (FAIL)
#   - Secret volumes should be mounted read-only (WARN)
#   - Debug/dev flags in ConfigMap data (INFO)

package artigen.policies.configmap_hygiene

import rego.v1

# - Category metadata  --------------
_category := "ConfigMaps/Secrets"
_reference := "https://kubernetes.io/docs/concepts/configuration/secret/"

_workload_kinds := {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}

_pod_spec(manifest) := manifest.spec.template.spec if {
	manifest.kind in _workload_kinds
}

# - Credential-like keys in ConfigMaps (FAIL) ----------
# Key names that strongly suggest secrets
_credential_keys := [
	"password",
	"passwd",
	"secret",
	"api_key",
	"apikey",
	"api-key",
	"token",
	"private_key",
	"private-key",
	"credentials",
]

# Suffixes/patterns that look like credentials but are actually config
# (e.g. SECRET_FETCH_RETRYCOUNT, TOKENENABLED, secretName references)
_false_positive_suffixes := [
	"retrycount",
	"retry_count",
	"timeout",
	"enabled",
	"disabled",
	"interval",
	"count",
	"size",
	"length",
	"max",
	"min",
	"port",
	"host",
	"path",
	"name",
	"secretname",
	"secretkey",
]

_is_false_positive(key) if {
	lower_key := lower(key)
	some suffix in _false_positive_suffixes
	endswith(lower_key, suffix)
}

# Keys ending with common reference patterns (pointing to secret names, not values)
_is_secret_reference(key) if {
	lower_key := lower(key)
	endswith(lower_key, "secretname")
}

_is_secret_reference(key) if {
	lower_key := lower(key)
	endswith(lower_key, "secretkey")
}

_is_secret_reference(key) if {
	lower_key := lower(key)
	endswith(lower_key, "secret_name")
}

_is_secret_reference(key) if {
	lower_key := lower(key)
	endswith(lower_key, "secret_key")
}

# Shell script filenames are not credentials
_is_script_filename(key) if {
	endswith(key, ".sh")
}

_is_script_filename(key) if {
	endswith(key, ".py")
}

_is_script_filename(key) if {
	endswith(key, ".yaml")
}

_key_looks_like_credential(key) if {
	lower_key := lower(key)
	some cred in _credential_keys
	contains(lower_key, cred)
	not _is_false_positive(key)
	not _is_secret_reference(key)
	not _is_script_filename(key)
}

violations contains violation if {
	some manifest in input.manifests
	manifest.kind == "ConfigMap"
	data := object.get(manifest, "data", {})
	some key, _ in data
	_key_looks_like_credential(key)

	violation := {
		"msg": sprintf("ConfigMap/%s: key '%s' looks like a credential - use a Secret instead", [manifest.metadata.name, key]),
		"severity": "fail",
		"category": _category,
		"objective": "Credentials must not be stored in ConfigMaps (use Secrets)",
		"expected_outcome": "No credential-like keys in ConfigMap data",
		"resource": {
			"kind": "ConfigMap",
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Move key '%s' from ConfigMap/%s to a Secret resource", [key, manifest.metadata.name]),
		"reference": _reference,
	}
}

# - Secret volumes not mounted read-only (WARN) ---------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	ps := _pod_spec(manifest)

	# Find volumes backed by secrets
	volumes := object.get(ps, "volumes", [])
	some vol in volumes
	object.get(vol, "secret", false) != false
	vol_name := vol.name

	# Find volumeMounts for this volume
	containers := object.get(ps, "containers", [])
	some container in containers
	mounts := object.get(container, "volumeMounts", [])
	some mount in mounts
	mount.name == vol_name
	ro := object.get(mount, "readOnly", false)
	ro != true

	violation := {
		"msg": sprintf("%s/%s container '%s': secret volume '%s' mounted as read-write", [manifest.kind, manifest.metadata.name, container.name, vol_name]),
		"severity": "warn",
		"category": _category,
		"objective": "Secret volumes should be mounted read-only",
		"expected_outcome": "volumeMount.readOnly is true for secret-backed volumes",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Set readOnly: true on volumeMount '%s' in container '%s'", [vol_name, container.name]),
		"reference": _reference,
	}
}

# - Debug/dev flags in ConfigMaps (INFO) -------------
_debug_keys := [
	"debug",
	"DEBUG",
	"verbose",
	"VERBOSE",
	"dev_mode",
	"DEV_MODE",
	"development",
]

_debug_values := [
	"true",
	"1",
	"yes",
	"on",
	"enabled",
]

violations contains violation if {
	some manifest in input.manifests
	manifest.kind == "ConfigMap"
	data := object.get(manifest, "data", {})
	some key, val in data
	some dk in _debug_keys
	contains(key, dk)
	lower_val := lower(val)
	some dv in _debug_values
	lower_val == dv

	violation := {
		"msg": sprintf("ConfigMap/%s: debug flag '%s=%s' is enabled", [manifest.metadata.name, key, val]),
		"severity": "info",
		"category": _category,
		"objective": "Debug/development flags should be disabled in production",
		"expected_outcome": "No debug/verbose/dev_mode flags set to true",
		"resource": {
			"kind": "ConfigMap",
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Set '%s' to false/0 in ConfigMap/%s for production", [key, manifest.metadata.name]),
		"reference": _reference,
	}
}
