#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
set -euo pipefail

script=$(cd "$(dirname "$0")" && pwd)/release-tag.sh
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
git init --bare --quiet "$scratch/remote.git"
git init --quiet -b main "$scratch/work"
cd "$scratch/work"
git config user.name 'Release Test'
git config user.email 'release@example.test'
git config commit.gpgsign false
git config tag.gpgsign false
git remote add origin "$scratch/remote.git"
git commit --quiet --allow-empty -m 'Initial fixture'
first=$(git rev-parse HEAD)
git push --quiet origin main

# The first release works without any existing tags and is annotated.
test "$(bash "$script" "$first")" = v0.0.1
test "$(git cat-file -t v0.0.1)" = tag
test "$(git --git-dir="$scratch/remote.git" rev-parse 'v0.0.1^{commit}')" = "$first"

# Sort versions numerically and ignore prereleases and unrelated tags.
git tag -a v1.2.9 -m 'Older release'
git tag -a v1.2.10 -m 'Latest release'
git tag -a v9.0.0-rc.1 -m 'Prerelease'
git tag -a unrelated -m 'Unrelated tag'
git push --quiet origin --tags
git commit --quiet --allow-empty -m 'Tested change'
tested=$(git rev-parse HEAD)
git push --quiet origin main
git commit --quiet --allow-empty -m 'Untested change'
test "$(bash "$script" "$tested")" = v1.2.11
test "$(git --git-dir="$scratch/remote.git" rev-parse 'v1.2.11^{commit}')" = "$tested"

# A retry reuses the tag; the following commit gets just one patch bump.
test "$(bash "$script" "$tested")" = v1.2.11
test "$(git tag --list v1.2.12)" = ''
git push --quiet origin main
test "$(bash "$script" HEAD)" = v1.2.12

# A late CI run or rerun must not publish old code as a newer version.
test "$(bash "$script" "$tested")" = ''
test "$(git tag --list v1.2.13)" = ''
git checkout --quiet -b divergent "$first"
git commit --quiet --allow-empty -m 'Unrelated change'
if bash "$script" HEAD; then
  echo 'released a commit outside the release history' >&2
  exit 1
fi
test "$(git tag --list v1.2.13)" = ''
echo 'Release tagging tests passed'
