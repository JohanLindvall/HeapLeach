#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
set -euo pipefail
version=${1:?usage: smoke-archive.sh VERSION}
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
for archive in dist/*.tar.gz dist/*.zip; do
  [ -f "$archive" ] || continue
  case "$archive" in
    *.zip) unzip -q "$archive" -d "$scratch" ;;
    *) tar -xzf "$archive" -C "$scratch" ;;
  esac
done
binary=$scratch/heapleach
if [ -f "$binary.exe" ]; then binary=$binary.exe; fi
test "$("$binary" -version 2>&1)" = "heapleach $version"
test -s "$scratch/ocr/LICENSE-model.txt"
test -s "$scratch/ocr/LICENSE-runtime.txt"
test -s "$scratch/ocr/ThirdPartyNotices.txt"
export HEAPLEACH_OCR_DIR="$scratch/ocr"
# Go on Windows expects a native path, unlike Git Bash's /tmp prefix.
if command -v cygpath >/dev/null 2>&1; then
  HEAPLEACH_OCR_DIR=$(cygpath -w "$HEAPLEACH_OCR_DIR")
fi
cd backend
go test ./internal/captcha -run TestModel -count=1 -v
