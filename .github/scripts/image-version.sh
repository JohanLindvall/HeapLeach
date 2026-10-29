#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Read-only version stamp for a main image, including an exactly tagged commit.
set -euo pipefail

commit=$(git rev-parse --verify "${1:-HEAD}^{commit}")
latest=$(git tag --merged "$commit" --sort=-version:refname --list 'v*' |
  sed -nE '/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/p' |
  sed -n '1p')
printf '%s-g%s\n' "${latest:-v0.0.0}" "$(git rev-parse --short=12 "$commit")"
