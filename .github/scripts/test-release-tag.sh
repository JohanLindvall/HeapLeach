#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
set -euo pipefail

scripts=$(cd "$(dirname "$0")" && pwd)
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

reject_tag() {
  if bash "$scripts/release-tag.sh" "$@" >"$scratch/rejected.log" 2>&1; then
    echo "unexpectedly accepted tag arguments: $*" >&2
    exit 1
  fi
}

# Image versions always include the commit and never allocate a tag.
short=$(git rev-parse --short=12 HEAD)
test "$(bash "$scripts/image-version.sh")" = "v0.0.0-g$short"
test -z "$(git tag)"
reject_tag
reject_tag "$first"
reject_tag v1.0.0
reject_tag '' "$first" invalid-run-id
for invalid in 1.2.3 v1.2 v01.2.3 v1.2.3-rc.1 v1.2.3+build 'v1.2.3 extra'; do
  reject_tag "$invalid" "$first"
done
test -z "$(git tag)"

# The first manual version is exactly the requested version, and annotated.
test "$(bash "$scripts/release-tag.sh" v1.2.9 "$first")" = v1.2.9
test "$(git cat-file -t v1.2.9)" = tag
test "$(git --git-dir="$scratch/remote.git" rev-parse 'v1.2.9^{commit}')" = "$first"
test "$(bash "$scripts/image-version.sh" HEAD)" = "v1.2.9-g$short"

# Versions sort numerically; unrelated tags and prereleases are ignored.
git tag -a v1.2.10 -m 'Latest stable fixture'
git tag -a v9.0.0-rc.1 -m 'Prerelease fixture'
git tag -a unrelated -m 'Unrelated fixture'
git push --quiet origin --tags
git commit --quiet --allow-empty -m 'Tested change'
tested=$(git rev-parse HEAD)
short=$(git rev-parse --short=12 HEAD)
git push --quiet origin main
before=$(git show-ref --tags)
test "$(bash "$scripts/image-version.sh" "$tested")" = "v1.2.10-g$short"
test "$(git show-ref --tags)" = "$before"
test "$(git --git-dir="$scratch/remote.git" show-ref --tags)" = "$before"

# A workflow tags its captured SHA even if main has advanced in the meantime.
git commit --quiet --allow-empty -m 'Following change'
git push --quiet origin main
test "$(bash "$scripts/release-tag.sh" v1.3.0 "$tested")" = v1.3.0
test "$(git --git-dir="$scratch/remote.git" rev-parse 'v1.3.0^{commit}')" = "$tested"
before=$(git show-ref --tags)
test "$(bash "$scripts/release-tag.sh" v1.3.0 "$tested")" = v1.3.0
reject_tag v1.3.0 HEAD
reject_tag v1.2.11 HEAD
reject_tag v1.4.0 "$first"
test "$(git show-ref --tags)" = "$before"

# A retry finishes a tag push that failed after local creation.
git tag -a v1.4.0 -m 'Locally created fixture'
test "$(bash "$scripts/release-tag.sh" v1.4.0 HEAD)" = v1.4.0
test "$(git --git-dir="$scratch/remote.git" rev-parse 'v1.4.0^{commit}')" = "$(git rev-parse HEAD)"

# A main image uses only tags reachable from its commit, even after a newer
# version is created or an unrelated branch receives a higher version.
git checkout --quiet -b divergent "$first"
git commit --quiet --allow-empty -m 'Unrelated change'
reject_tag v2.0.0 HEAD
git tag -a v99.0.0 -m 'Unmerged version fixture'
git push --quiet origin --tags
test "$(bash "$scripts/image-version.sh" "$tested")" = "v1.3.0-g$short"
test -z "$(git tag --list v2.0.0)"

# A blank version starts at v0.0.1, then bumps the latest stable patch.
default_remote=$scratch/default.git
git init --bare --quiet "$default_remote"
git init --quiet -b main "$scratch/default-work"
cd "$scratch/default-work"
git config user.name 'Release Test'
git config user.email 'release@example.test'
git config commit.gpgsign false
git config tag.gpgsign false
git remote add origin "$default_remote"
git commit --quiet --allow-empty -m 'Default version fixture'
default_first=$(git rev-parse HEAD)
git push --quiet origin main
test "$(bash "$scripts/release-tag.sh" '' HEAD 1001)" = v0.0.1
test "$(git cat-file -t v0.0.1)" = tag
test "$(git --git-dir="$default_remote" rev-parse 'v0.0.1^{commit}')" = "$default_first"

# A fresh runner finds the original run's tag in the remote annotation.
git tag -d v0.0.1 >/dev/null
test "$(bash "$scripts/release-tag.sh" '' HEAD 1001)" = v0.0.1
test -z "$(git tag --list v0.0.2)"

# Another manual dispatch bumps even at the same SHA. Retrying the older
# dispatch still uses its own version, not the latest tag or a new patch.
test "$(bash "$scripts/release-tag.sh" '' HEAD 1002)" = v0.0.2
before=$(git show-ref --tags)
test "$(bash "$scripts/release-tag.sh" '' HEAD 1001)" = v0.0.1
test "$(git show-ref --tags)" = "$before"

# Explicit versions override the default; later defaults follow that version
# numerically and ignore unrelated tags and prereleases.
test "$(bash "$scripts/release-tag.sh" v2.4.9 HEAD 1003)" = v2.4.9
git tag -a v9.0.0-rc.1 -m 'Prerelease fixture'
git tag -a unrelated -m 'Unrelated fixture'
git push --quiet origin --tags
test "$(bash "$scripts/release-tag.sh" '' HEAD 1004)" = v2.4.10

# Defaults also tag the captured commit, and reject a new run on old code.
git commit --quiet --allow-empty -m 'Default patch change'
default_tested=$(git rev-parse HEAD)
git commit --quiet --allow-empty -m 'Following default change'
git push --quiet origin main
test "$(bash "$scripts/release-tag.sh" '' "$default_tested" 1005)" = v2.4.11
test "$(git --git-dir="$default_remote" rev-parse 'v2.4.11^{commit}')" = "$default_tested"
before=$(git show-ref --tags)
test "$(bash "$scripts/release-tag.sh" '' "$default_first" 1004)" = v2.4.10
reject_tag '' "$default_first" 1006
test "$(git show-ref --tags)" = "$before"
test "$(bash "$scripts/release-tag.sh" '' HEAD)" = v2.4.12

echo 'Image version and manual tagging tests passed'
