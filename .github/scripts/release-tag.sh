#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Called after CI, under release.yml's publication lock. Stdout is the tag
# to publish, or empty when a newer release already contains this commit.
set -euo pipefail

commit=$(git rev-parse "${1:?usage: release-tag.sh <tested-commit>}^{commit}")
git fetch --quiet origin --tags
mapfile -t tags < <(git tag --sort=-version:refname --list 'v*' |
  sed -nE '/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/p')
latest=${tags[0]:-v0.0.0}

if [ "${#tags[@]}" -gt 0 ]; then
  released=$(git rev-parse "$latest^{commit}")
  if [ "$commit" = "$released" ]; then
    printf '%s\n' "$latest"
    exit 0
  fi
  if git merge-base --is-ancestor "$commit" "$released"; then
    echo "$latest already contains $commit; no new release needed" >&2
    exit 0
  fi
  if ! git merge-base --is-ancestor "$released" "$commit"; then
    echo "refusing to release a commit that does not descend from $latest" >&2
    exit 1
  fi
fi

IFS=. read -r major minor patch <<< "${latest#v}"
tag="v$major.$minor.$((patch + 1))"
git -c user.name='github-actions[bot]' \
    -c user.email='41898282+github-actions[bot]@users.noreply.github.com' \
    tag -a "$tag" "$commit" -m "HeapLeach $tag"
git push origin "refs/tags/$tag" >&2
printf '%s\n' "$tag"
