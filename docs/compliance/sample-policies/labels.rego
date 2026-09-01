# Required Labels Policy
#
# Ensures all workload resources carry the 6 standard Kubernetes recommended
# labels (app.kubernetes.io/*).
#
# Severity is WARN - vendor Helm charts may not include all labels and
# need upstream fixes.

package artigen.policies.labels

import rego.v1

# - Category metadata  --------------
_category := "Labels (Application)"
_reference := "https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/"

_workload_kinds := ["Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"]

_required_labels := [
	"app.kubernetes.io/name",
	"app.kubernetes.io/instance",
	"app.kubernetes.io/version",
	"app.kubernetes.io/component",
	"app.kubernetes.io/part-of",
	"app.kubernetes.io/managed-by",
]

# - Missing required label --------------------â”€
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds

	labels := object.get(manifest, ["metadata", "labels"], {})
	some required in _required_labels
	not labels[required]

	violation := {
		"msg": sprintf("%s/%s is missing required label '%s'", [manifest.kind, manifest.metadata.name, required]),
		"severity": "warn",
		"category": _category,
		"objective": "Every workload carries the 6 standard app.kubernetes.io/* labels",
		"expected_outcome": "All 6 recommended labels present on metadata.labels",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Add label '%s' to metadata.labels", [required]),
		"reference": _reference,
	}
}

# - Label value too long (>63 chars) ---------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds

	labels := object.get(manifest, ["metadata", "labels"], {})
	some key, val in labels
	startswith(key, "app.kubernetes.io/")
	count(val) > 63

	violation := {
		"msg": sprintf("Label '%s' on %s/%s has value longer than 63 chars", [key, manifest.kind, manifest.metadata.name]),
		"severity": "warn",
		"category": _category,
		"objective": "Label values must be DNS-safe (max 63 characters)",
		"expected_outcome": "Label value length <= 63 characters",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Shorten the value of label '%s' to 63 characters or fewer", [key]),
		"reference": _reference,
	}
}

# - Pod template label mismatch -----------------â”€
# For workloads with pod templates, the template's labels should mirror
# the top-level metadata labels for the required set.
_template_kinds := ["Deployment", "StatefulSet", "DaemonSet"]

violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _template_kinds

	top_labels := object.get(manifest, ["metadata", "labels"], {})
	pod_labels := object.get(manifest, ["spec", "template", "metadata", "labels"], {})

	some required in _required_labels
	top_val := top_labels[required]
	pod_val := object.get(pod_labels, required, "")
	top_val != pod_val

	violation := {
		"msg": sprintf("Label '%s' on %s/%s differs between metadata and pod template", [required, manifest.kind, manifest.metadata.name]),
		"severity": "warn",
		"category": _category,
		"objective": "Pod template labels mirror top-level metadata labels",
		"expected_outcome": "Matching label values between metadata.labels and spec.template.metadata.labels",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Sync label '%s' in spec.template.metadata.labels to match metadata.labels", [required]),
		"reference": _reference,
	}
}
