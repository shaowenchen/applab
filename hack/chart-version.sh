#!/usr/bin/env bash
#
# The version this commit publishes, as one string.
#
# It exists because two workflows need the same answer and used to derive it
# separately: the chart is packaged under this version, and the image is tagged
# with it — and the chart's default image tag is its own version. When those were
# computed in two places they disagreed, and nothing noticed: chart.yml stamped
# the bare commit (`6ac4468`) into appVersion while image.yml published the image
# as `sha-6ac4468`. The chart's default tag therefore named a tag that does not
# exist, and every install that did not override `image.tag` — including the one
# in the chart's own README — pulled nothing.
#
# A tag names the release, so it wins; anything else is a development build and
# takes the chart's own version with "-dev". The version is fixed rather than
# derived from the commit, so repeated installs of a dev build take the newest
# one instead of accumulating a listing entry per commit.
#
# The counterpart to a moving tag is `imagePullPolicy: Always`, which the chart
# sets: a node that already has `0.1.0-dev` would otherwise keep running the
# image it cached, and an install that reports success would serve an older
# build.
#
# Usage: chart-version.sh            # the version this commit publishes
set -euo pipefail

cd "$(dirname "$0")/.."

version="$(grep '^version:' charts/applab/Chart.yaml | awk '{print $2}')"
if [ -z "$version" ]; then
  echo "no version found in charts/applab/Chart.yaml" >&2
  exit 1
fi

# In CI the ref is given rather than discovered: a checkout is often detached, so
# `git describe` would answer about the wrong commit if it answered at all.
if [ -n "${GITHUB_REF_TYPE:-}" ]; then
  if [ "$GITHUB_REF_TYPE" = "tag" ]; then
    echo "${GITHUB_REF_NAME#v}"
  else
    echo "${version}-dev"
  fi
  exit 0
fi

# Locally: an exact tag on HEAD is a release, anything else is a dev build.
if tag="$(git describe --tags --exact-match 2>/dev/null)"; then
  echo "${tag#v}"
else
  echo "${version}-dev"
fi
