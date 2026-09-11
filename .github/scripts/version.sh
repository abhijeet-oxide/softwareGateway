#!/usr/bin/env bash
# THE VERSION, AND WHICH IMAGES IT NEEDS.
#
# Two numbers come out of here and they are not the same number:
#
#   chart_version   the release. It moves on EVERY merge - code, a product, a
#                   person, a template. It is what a HelmRelease points at.
#   image_version   the tag all three images carry. It moves ONLY when code
#                   changes, so a release that adds a product ships a new chart
#                   that reuses the running images - and a rollout with no new
#                   image is a rollout with no restarts.
#
# # How image_version is found without storing anything
#
# The naive way is to remember the last version that had a build, which means a
# file in the repository that a bot edits, which means merge conflicts between
# branches and a value that can be wrong. There is a better answer, and it is
# already in the repository:
#
#   1. CODE_REV = the last commit that touched anything an image is built from.
#   2. The releases that CONTAIN that commit are `git tag --contains CODE_REV`.
#   3. The OLDEST of them is the release that first shipped this code - so its
#      images exist, and they are these images.
#   4. No tag contains it, so this release is the first to ship it: build.
#
# Nothing is stored, nothing can drift, and two people running this on the same
# commit get the same answer.
#
# # Why that also makes production run the bits lab ran
#
# Lab tags releases too (`v1.4.3-lab.87`). When that commit merges to main,
# step 3 finds the lab tag - it is older and lower - so production deploys the
# image lab tested rather than a rebuild of the same source. There is no
# promotion step to get wrong, because there is nothing to promote.
#
# THE ONE CASE WHERE THAT DOES NOT HOLD is a squash merge: it creates a new
# commit, no tag contains it, and the images are rebuilt. The source is
# identical so the software is, but the digests differ. Merge lab into main
# with a merge commit if you want artefact identity end to end.
#
# Usage:  CHANNEL=main|lab .github/scripts/version.sh
# Writes  key=value lines to stdout, and to $GITHUB_OUTPUT when set.
set -euo pipefail

CHANNEL="${CHANNEL:?CHANNEL must be main or lab}"
RUN="${GITHUB_RUN_NUMBER:-0}"

# ANYTHING AN IMAGE IS BUILT FROM. Adding a path here is how a new input starts
# triggering rebuilds; forgetting one is how a code change ships inside an old
# image, so deploy/deploy_test.go asserts this list against the Dockerfiles'
# COPY instructions.
CODE_PATHS=(
  cmd internal pkg web
  go.mod go.sum
  deploy/build deploy/web deploy/certs deploy/go deploy/npm
)

die() { echo "version.sh: $*" >&2; exit 1; }

git rev-parse --git-dir >/dev/null 2>&1 || die "not a git repository"

# --- 1. the last released version ---------------------------------------------
# Release tags only. A prerelease (`v1.4.3-lab.87`) is a preview OF 1.4.3, so
# basing the next bump on one would produce 1.4.4 and skip the release it was
# previewing.
last="$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname \
        | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1 || true)"

if [ -z "$last" ]; then
  # A repository with no releases yet. 0.1.0 rather than 1.0.0: the first
  # number a pipeline invents should not be the one that claims stability.
  next="0.1.0"
  bump="initial"
else
  base="${last#v}"
  IFS=. read -r major minor patch <<<"$base"

  # HOW A MINOR OR MAJOR IS ASKED FOR. A trailer in a commit message on the way
  # in - which is reviewable, lives with the change that earned it, and needs
  # no label that can be added after the fact:
  #
  #   Release: minor
  #
  # Anything else is a patch, which is the right default for a system whose
  # releases are mostly configuration.
  range="$last..HEAD"
  msgs="$(git log --format='%B' "$range" || true)"
  if grep -qiE '^Release:[[:space:]]*major' <<<"$msgs"; then
    major=$((major + 1)); minor=0; patch=0; bump="major"
  elif grep -qiE '^Release:[[:space:]]*minor' <<<"$msgs"; then
    minor=$((minor + 1)); patch=0; bump="minor"
  else
    patch=$((patch + 1)); bump="patch"
  fi
  next="${major}.${minor}.${patch}"
fi

case "$CHANNEL" in
  main) chart_version="$next" ;;
  # A PRERELEASE, and SemVer orders it before the release it previews - which
  # is exactly what it is. The run number rather than a count of commits: it is
  # monotonic, unique, and does not change when history does.
  lab)  chart_version="${next}-lab.${RUN}" ;;
  *)    die "CHANNEL must be main or lab, got '$CHANNEL'" ;;
esac

# --- 2. the last commit that touched an image ---------------------------------
code_rev="$(git log -1 --format=%H -- "${CODE_PATHS[@]}" || true)"
[ -n "$code_rev" ] || die "no commit touches ${CODE_PATHS[*]} - is this a shallow clone? fetch-depth: 0 is required"

# --- 3. the release that first shipped it -------------------------------------
containing="$(git tag --contains "$code_rev" --list 'v[0-9]*' --sort=v:refname | head -1 || true)"

if [ -n "$containing" ]; then
  image_version="${containing#v}"
  images_changed="false"
  reason="code unchanged since ${containing}; reusing its images"
else
  image_version="$chart_version"
  images_changed="true"
  reason="code changed at ${code_rev:0:12}; building"
fi

emit() {
  echo "$1=$2"
  [ -n "${GITHUB_OUTPUT:-}" ] && echo "$1=$2" >> "$GITHUB_OUTPUT"
  return 0
}

emit chart_version  "$chart_version"
emit image_version  "$image_version"
emit images_changed "$images_changed"
emit code_rev       "$code_rev"
emit previous_tag   "${last:-none}"
emit bump           "$bump"
emit tag            "v${chart_version}"

# The summary is the thing somebody reads when they are asking why production
# is on the version it is on.
{
  echo "### Release ${chart_version}"
  echo
  echo "| | |"
  echo "|---|---|"
  echo "| chart version | \`${chart_version}\` (${bump} from \`${last:-none}\`) |"
  echo "| image version | \`${image_version}\` |"
  echo "| images | ${reason} |"
} >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
