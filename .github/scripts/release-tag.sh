#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Called only by the manual version workflow. Never chooses or increments a
# version; stdout is the requested tag after it has been pushed successfully.
set -euo pipefail

tag=${1:?usage: release-tag.sh <vX.Y.Z> <tested-commit>}
if [[ ! "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo 'version must be a stable tag in vX.Y.Z format' >&2
  exit 1
fi
commit=$(git rev-parse --verify "${2:?usage: release-tag.sh <vX.Y.Z> <tested-commit>}^{commit}")
git fetch --quiet origin --tags '+refs/heads/main:refs/remotes/origin/main'
if ! git merge-base --is-ancestor "$commit" origin/main; then
  echo 'refusing to tag a commit outside main' >&2
  exit 1
fi

if git show-ref --verify --quiet "refs/tags/$tag"; then
  if [ "$(git rev-parse "$tag^{commit}")" != "$commit" ]; then
    echo "refusing to move existing tag $tag to another commit" >&2
    exit 1
  fi
else
  latest=$(git tag --sort=-version:refname --list 'v*' |
    sed -nE '/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/p' |
    sed -n '1p')
  if [ -n "$latest" ]; then
    IFS=. read -r major minor patch <<< "${latest#v}"
    IFS=. read -r next_major next_minor next_patch <<< "${tag#v}"
    if ! (( next_major > major ||
            (next_major == major && next_minor > minor) ||
            (next_major == major && next_minor == minor && next_patch > patch) )); then
      echo "version must be newer than $latest" >&2
      exit 1
    fi
    if ! git merge-base --is-ancestor "$latest^{commit}" "$commit"; then
      echo "refusing to tag a commit that does not descend from $latest" >&2
      exit 1
    fi
  fi

  git -c user.name='github-actions[bot]' \
      -c user.email='41898282+github-actions[bot]@users.noreply.github.com' \
      tag -a "$tag" "$commit" -m "HeapLeach $tag"
fi
# Also push on retries: the prior attempt may have created only a local tag.
git push origin "refs/tags/$tag" >&2
printf '%s\n' "$tag"
