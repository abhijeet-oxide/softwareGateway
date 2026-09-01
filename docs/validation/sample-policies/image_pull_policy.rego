# Image Pull Policy
#
# Checks:
#   - imagePullPolicy should be 'Always' for mutable tags (WARN)
#   - :latest tag is always a FAIL
#   - Digest-pinned images can safely use IfNotPresent (PASS)
#
# Rationale: Mutable tags (anything that isn't a digest) can change
# between pulls. 'Always' ensures the node fetches the current image.
# :latest is inherently unpredictable in production.

package artigen.policies.image_pull_policy

import rego.v1

_category := "Security (Runtime & Supply Chain)"
_reference := "https://kubernetes.io/docs/concepts/containers/images/#image-pull-policy"

_workload_kinds := {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}

_pod_spec(manifest) := manifest.spec.template.spec if {
	manifest.kind in _workload_kinds
}

_all_containers(manifest) := array.concat(
	object.get(_pod_spec(manifest), "containers", []),
	object.get(_pod_spec(manifest), "initContainers", []),
)

# Image uses a digest (contains @sha256:)
_is_digest_pinned(image) if {
	contains(image, "@sha256:")
}

# Image uses :latest tag
_uses_latest(image) if {
	endswith(image, ":latest")
}

_uses_latest(image) if {
	# No tag at all - defaults to :latest
	not contains(image, ":")
}

_uses_latest(image) if {
	# Has @ but also :latest before it
	parts := split(image, "@")
	endswith(parts[0], ":latest")
}

# - :latest tag (FAIL) ----------------------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	some container in _all_containers(manifest)
	_uses_latest(container.image)

	violation := {
		"msg": sprintf("Container '%s' in %s/%s uses ':latest' tag - unpredictable in production", [container.name, manifest.kind, manifest.metadata.name]),
		"severity": "fail",
		"category": _category,
		"objective": "Never use :latest tag in production deployments",
		"expected_outcome": "Image uses a specific version tag or digest",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Pin container '%s' image to a specific version tag (e.g. :1.2.3) or digest (@sha256:...)", [container.name]),
		"reference": _reference,
	}
}

# - imagePullPolicy not Always for mutable tags (WARN) ------
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds
	some container in _all_containers(manifest)
	not _is_digest_pinned(container.image)
	not _uses_latest(container.image)
	policy := object.get(container, "imagePullPolicy", "IfNotPresent")
	policy != "Always"

	violation := {
		"msg": sprintf("Container '%s' in %s/%s uses mutable tag with imagePullPolicy='%s'", [container.name, manifest.kind, manifest.metadata.name, policy]),
		"severity": "warn",
		"category": _category,
		"objective": "imagePullPolicy should be 'Always' for mutable (non-digest) images",
		"expected_outcome": "imagePullPolicy is 'Always' or image is digest-pinned",
		"resource": {
			"kind": manifest.kind,
			"namespace": object.get(manifest.metadata, "namespace", ""),
			"name": manifest.metadata.name,
		},
		"remediation": sprintf("Set imagePullPolicy: Always on container '%s', or pin the image by digest", [container.name]),
		"reference": _reference,
	}
}
