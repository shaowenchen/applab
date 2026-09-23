#!/usr/bin/env bash
#
# Package the chart into a directory that is also served as a Helm repository.
#
# The directory is a working copy of gh-pages: whatever is already in it is what
# is already published, and the index is regenerated from what ends up in it.
# That is deliberate — building the index from the directory's contents rather
# than merging into an existing index is what keeps a single source of truth.
# Two publishers merging into one index (a release tool and this) is how an
# entry silently disappears from a repository listing.
#
# Usage: package-chart.sh <version> <app-version> <pages-dir> <repo-url>
#
# Local use is the same command with a directory of your own, which is what
# makes this testable without a cluster or a GitHub runner.
set -euo pipefail

version="${1:?usage: package-chart.sh <version> <app-version> <pages-dir> <repo-url>}"
app_version="${2:?}"
pages_dir="${3:?}"
repo_url="${4:?}"

cd "$(dirname "$0")/.."

if [ ! -d "$pages_dir" ]; then
  echo "pages directory $pages_dir does not exist" >&2
  exit 1
fi

# A leading "v" is a git tag convention, not a Helm one.
version="${version#v}"

package="applab-${version}.tgz"

# Only one development build is kept. Its version is fixed so that installing
# the chart repeatedly takes the newest build rather than accumulating an entry
# per commit — and a repository listing with a hundred indistinguishable
# 0.1.0-dev-abc123 rows in it is worse than useless.
#
# Released versions are left alone: pruning is limited to the "-dev" suffix, so
# a dev build can never delete a real release.
for stale in "$pages_dir"/*-dev.tgz; do
  [ -e "$stale" ] || continue
  [ "$(basename "$stale")" = "$package" ] && continue
  echo "  removing superseded dev package $(basename "$stale")"
  rm -f "$stale"
done

echo "  packaging chart $version (app $app_version)"
helm package charts/applab \
  --version "$version" \
  --app-version "$app_version" \
  --destination "$pages_dir"

# Regenerated from the directory rather than merged into the old index, so the
# index cannot list a package that is not there or omit one that is.
helm repo index "$pages_dir" --url "$repo_url"

echo "  published packages:"
ls -1 "$pages_dir"/*.tgz | sed 's|.*/|    |'
