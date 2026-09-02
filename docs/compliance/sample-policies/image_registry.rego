# Image Registry & Tag Policy
#
# Ensures all container images come from approved registries only,
# disallows :latest tags, requires either a versioned tag or digest,
# and recommends digest pinning for production immutability.
#
# Approved registry rules:
#
# 1. NCD Harbor
#    Example:
#      harbor.wst2m0ncdx0001c.oamwnvlil.itn.att.com
#    Rule:
#      registry host must start with "harbor"
#      and end with "itn.att.com"
#
# 2. Quay Registry
#    Example:
#      quay-registry.apps.wnv6h1.itn.3pc.att.com
#    Rule:
#      registry host must start with "quay-registry.apps."
#      and end with "itn.3pc.att.com"
#
# 3. JFrog
#    Exact:
#      artifact.it.att.com
#
# 4. Local ITMS
#    Exact:
#      quay.local
#
# Non-approved external registries are not allowed.

package artigen.policies.image_registry

import rego.v1

# ── Category metadata validators.csv ──────────────────────────────
_category := "Security (Runtime & Supply Chain)"
_reference := "https://kubernetes.io/docs/concepts/containers/images/#image-names"

# ── Approved exact registry hosts ─────────────────────────────────
_approved_exact_hosts := {
	"artifact.it.att.com",
	"quay.local",
}

# ── Kubernetes workload kinds evaluated by this policy ────────────
_workload_kinds := {
	"Pod",
	"Deployment",
	"StatefulSet",
	"DaemonSet",
	"Job",
	"CronJob",
}

# ── Pod spec extraction ───────────────────────────────────────────
_pod_spec(manifest) := podspec if {
	manifest.kind == "Pod"
	podspec := object.get(manifest, "spec", {})
}

_pod_spec(manifest) := podspec if {
	manifest.kind == "CronJob"
	podspec := object.get(manifest, ["spec", "jobTemplate", "spec", "template", "spec"], {})
}

_pod_spec(manifest) := podspec if {
	manifest.kind != "Pod"
	manifest.kind != "CronJob"
	podspec := object.get(manifest, ["spec", "template", "spec"], {})
}

_all_containers(manifest) := containers if {
	podspec := _pod_spec(manifest)

	containers_regular := object.get(podspec, "containers", [])
	containers_init := object.get(podspec, "initContainers", [])
	containers_ephemeral := object.get(podspec, "ephemeralContainers", [])

	containers_regular_init := array.concat(containers_regular, containers_init)
	containers := array.concat(containers_regular_init, containers_ephemeral)
}

# ── Safe metadata helpers ─────────────────────────────────────────
_manifest_name(manifest) := name if {
	metadata := object.get(manifest, "metadata", {})
	name := object.get(metadata, "name", "<unknown>")
}

_manifest_namespace(manifest) := namespace if {
	metadata := object.get(manifest, "metadata", {})
	default_namespace := object.get(input, "namespace", "default")
	namespace := object.get(metadata, "namespace", default_namespace)
}

_container_name(container) := name if {
	name := object.get(container, "name", "<unnamed>")
}

_container_image(container) := image if {
	image := object.get(container, "image", "")
}

# ── Image parsing helpers ─────────────────────────────────────────
#
# Image examples:
#   artifact.it.att.com/team/app:1.2.3
#   quay.local/team/app@sha256:<digest>
#   harbor.wst2m0ncdx0001c.oamwnvlil.itn.att.com/team/app:1.2.3
#   quay-registry.apps.wnv6h1.itn.3pc.att.com/team/app:1.2.3
#
# Registry host is the first path segment before the first "/".
# Bare image names without a registry host are considered unapproved.

_image_registry_host(image) := host if {
	parts := split(image, "/")
	count(parts) > 1
	host := parts[0]
}

_image_registry_host(image) := "" if {
	parts := split(image, "/")
	count(parts) <= 1
}

_image_without_digest(image) := image_without_digest if {
	parts := split(image, "@")
	image_without_digest := parts[0]
}

_image_last_path_segment(image) := segment if {
	image_without_digest := _image_without_digest(image)
	parts := split(image_without_digest, "/")
	segment := parts[count(parts) - 1]
}

_image_tag(image) := tag if {
	segment := _image_last_path_segment(image)
	contains(segment, ":")
	parts := split(segment, ":")
	tag := parts[count(parts) - 1]
}

_image_tag(image) := "" if {
	segment := _image_last_path_segment(image)
	not contains(segment, ":")
}

_image_has_sha256_digest(image) if {
	contains(image, "@sha256:")
}

# ── Approved registry checks ──────────────────────────────────────
_registry_host_exact_approved(host) if {
	host in _approved_exact_hosts
}

_registry_host_harbor_approved(host) if {
	startswith(host, "harbor")
	endswith(host, "itn.att.com")
}

_registry_host_quay_approved(host) if {
	startswith(host, "quay-registry.apps.")
	endswith(host, "itn.3pc.att.com")
}

_registry_host_approved(host) if {
	_registry_host_exact_approved(host)
}

_registry_host_approved(host) if {
	_registry_host_harbor_approved(host)
}

_registry_host_approved(host) if {
	_registry_host_quay_approved(host)
}

_image_from_approved_registry(image) if {
	host := _image_registry_host(image)
	host != ""
	_registry_host_approved(host)
}

# ── Missing image field ───────────────────────────────────────────
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds

	some container in _all_containers(manifest)
	image := _container_image(container)
	image == ""

	violation := {
		"msg": sprintf("Container '%s' in %s/%s is missing an image", [
			_container_name(container),
			manifest.kind,
			_manifest_name(manifest),
		]),
		"severity": "fail",
		"category": _category,
		"objective": "Ensure every container declares an image from an approved registry",
		"expected_outcome": "Container image is present and sourced from an approved registry",
		"resource": {
			"kind": manifest.kind,
			"namespace": _manifest_namespace(manifest),
			"name": _manifest_name(manifest),
		},
		"remediation": sprintf("Add an approved image reference for container '%s'", [
			_container_name(container),
		]),
		"reference": _reference,
	}
}

# ── Unapproved registry ───────────────────────────────────────────
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds

	some container in _all_containers(manifest)
	image := _container_image(container)
	image != ""

	not _image_from_approved_registry(image)

	violation := {
		"msg": sprintf("Container '%s' in %s/%s uses unapproved image registry: %s", [
			_container_name(container),
			manifest.kind,
			_manifest_name(manifest),
			image,
		]),
		"severity": "fail",
		"category": _category,
		"objective": "Ensure all container images come only from approved registries",
		"expected_outcome": "Image registry is one of: quay.local, artifact.it.att.com, harbor*.itn.att.com, or quay-registry.apps.*.itn.3pc.att.com",
		"resource": {
			"kind": manifest.kind,
			"namespace": _manifest_namespace(manifest),
			"name": _manifest_name(manifest),
		},
		"remediation": "Use an approved image registry: quay.local, artifact.it.att.com, harbor*.itn.att.com, or quay-registry.apps.*.itn.3pc.att.com. External/non-approved registries are not allowed.",
		"reference": _reference,
	}
}

# ── Disallow :latest tag ──────────────────────────────────────────
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds

	some container in _all_containers(manifest)
	image := _container_image(container)
	image != ""

	tag := _image_tag(image)
	tag == "latest"

	violation := {
		"msg": sprintf("Container '%s' in %s/%s uses disallowed ':latest' tag: %s", [
			_container_name(container),
			manifest.kind,
			_manifest_name(manifest),
			image,
		]),
		"severity": "fail",
		"category": _category,
		"objective": "Disallow mutable image tags",
		"expected_outcome": "Image uses a versioned tag or digest instead of ':latest'",
		"resource": {
			"kind": manifest.kind,
			"namespace": _manifest_namespace(manifest),
			"name": _manifest_name(manifest),
		},
		"remediation": sprintf("Pin container '%s' to a specific version tag or digest instead of ':latest'", [
			_container_name(container),
		]),
		"reference": _reference,
	}
}

# ── No tag and no digest ──────────────────────────────────────────
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds

	some container in _all_containers(manifest)
	image := _container_image(container)
	image != ""

	tag := _image_tag(image)
	tag == ""
	not _image_has_sha256_digest(image)

	violation := {
		"msg": sprintf("Container '%s' in %s/%s has no image tag or digest: %s", [
			_container_name(container),
			manifest.kind,
			_manifest_name(manifest),
			image,
		]),
		"severity": "fail",
		"category": _category,
		"objective": "Require versioned image references",
		"expected_outcome": "Image uses a versioned tag or sha256 digest",
		"resource": {
			"kind": manifest.kind,
			"namespace": _manifest_namespace(manifest),
			"name": _manifest_name(manifest),
		},
		"remediation": sprintf("Add a version tag or digest to the image for container '%s'", [
			_container_name(container),
		]),
		"reference": _reference,
	}
}

# ── Digest pinning advisory ───────────────────────────────────────
violations contains violation if {
	some manifest in input.manifests
	manifest.kind in _workload_kinds

	some container in _all_containers(manifest)
	image := _container_image(container)
	image != ""

	not _image_has_sha256_digest(image)

	tag := _image_tag(image)
	tag != ""
	tag != "latest"

	violation := {
		"msg": sprintf("Container '%s' in %s/%s uses a version tag without digest pinning: %s", [
			_container_name(container),
			manifest.kind,
			_manifest_name(manifest),
			image,
		]),
		"severity": "info",
		"category": _category,
		"objective": "Pin production images by digest for immutable deployments",
		"expected_outcome": "Image reference includes @sha256:<digest>",
		"resource": {
			"kind": manifest.kind,
			"namespace": _manifest_namespace(manifest),
			"name": _manifest_name(manifest),
		},
		"remediation": sprintf("Consider pinning container '%s' image by digest, for example image:tag@sha256:<digest>", [
			_container_name(container),
		]),
		"reference": _reference,
	}
}