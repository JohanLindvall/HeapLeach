#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Called only by the manual version workflow. An empty version bumps the
# latest stable patch; a workflow run ID makes retries reuse their first tag.
# Stdout is the selected tag after it has been pushed successfully.
set -euo pipefail

tag=${1-}
if [[ -n "$tag" && ! "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo 'version must be a stable tag in vX.Y.Z format' >&2
  exit 1
fi
commit=$(git rev-parse --verify "${2:?usage: release-tag.sh <vX.Y.Z-or-empty> <tested-commit> [workflow-run-id]}^{commit}")
run_id=${3-}
if [[ -n "$run_id" && ! "$run_id" =~ ^[0-9]+$ ]]; then
  echo 'workflow run ID must be numeric' >&2
  exit 1
fi
git fetch --quiet origin --tags '+refs/heads/main:refs/remotes/origin/main'
if ! git merge-base --is-ancestor "$commit" origin/main; then
  echo 'refusing to tag a commit outside main' >&2
  exit 1
fi

latest=$(git tag --sort=-version:refname --list 'v*' |
  sed -nE '/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/p' |
  sed -n '1p')

if [ -z "$tag" ] && [ -n "$run_id" ]; then
  # Tag annotations survive runner replacement and newer version updates.
  # GITHUB_RUN_ID stays the same when a workflow is rerun; a new dispatch
  # gets another ID and therefore another patch version, even at the same SHA.
  while IFS= read -r candidate; do
    if git for-each-ref --format='%(contents)' "refs/tags/$candidate" |
        grep -Fx "Workflow run: $run_id" >/dev/null; then
      tag=$candidate
      break
    fi
  done < <(git tag --points-at "$commit" --sort=-version:refname --list 'v*' |
    sed -nE '/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/p')
fi

if [ -z "$tag" ]; then
  base=${latest:-v0.0.0}
  IFS=. read -r major minor patch <<< "${base#v}"
  tag="v$major.$minor.$((patch + 1))"
fi

if git show-ref --verify --quiet "refs/tags/$tag"; then
  if [ "$(git rev-parse "$tag^{commit}")" != "$commit" ]; then
    echo "refusing to move existing tag $tag to another commit" >&2
    exit 1
  fi
else
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

  annotation=( -m "HeapLeach $tag" )
  if [ -n "$run_id" ]; then
    annotation+=( -m "Workflow run: $run_id" )
  fi
  git -c user.name='github-actions[bot]' \
      -c user.email='41898282+github-actions[bot]@users.noreply.github.com' \
      tag -a "$tag" "$commit" "${annotation[@]}"
fi
# Also push on retries: the prior attempt may have created only a local tag.
git push origin "refs/tags/$tag" >&2
printf '%s\n' "$tag"
