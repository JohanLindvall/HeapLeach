#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Install pinned OCR data and the CPU runtime. No Python installation is needed.
set -euo pipefail

target=${1:?usage: install.sh OS/ARCH DESTINATION}
dest=${2:?usage: install.sh OS/ARCH DESTINATION}
version=1.23.2
# 1.23.2 includes Intel macOS, which later upstream binary releases dropped.
# The Go binding uses this version's C API on every supported platform.
case "$target" in
  linux/amd64) platform=linux-x64; library=libonnxruntime.so ;;
  linux/arm64) platform=linux-arm64; library=libonnxruntime.so ;;
  darwin/amd64) platform=osx-x64; library=libonnxruntime.dylib ;;
  darwin/arm64) platform=osx-arm64; library=libonnxruntime.dylib ;;
  windows/amd64) platform=win-x64; library=onnxruntime.dll ;;
  *) echo "no OCR runtime for $target" >&2; exit 1 ;;
esac

cache=${HEAPLEACH_OCR_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/heapleach/ocr}
mkdir -p "$cache" "$dest"
scratch=$(mktemp -d "$dest/.install-XXXXXXXX")
trap 'rm -rf "$scratch"' EXIT

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d ' ' -f 1
  else
    shasum -a 256 "$1" | cut -d ' ' -f 1
  fi
}

fetch() {
  local url=$1 sum=$2 file=$3
  if [ -f "$file" ] && [ "$(sha256 "$file")" = "$sum" ]; then return; fi
  # Unique temporary files also allow independent builds to share the cache.
  local temporary
  temporary=$(mktemp "$cache/download-XXXXXXXX")
  if ! curl --fail --location --retry 3 --silent --show-error "$url" -o "$temporary"; then
    rm -f "$temporary"
    return 1
  fi
  if [ "$(sha256 "$temporary")" != "$sum" ]; then
    echo "checksum mismatch for $url" >&2
    rm -f "$temporary"
    return 1
  fi
  mv -f "$temporary" "$file"
}

wheel=ddddocr-1.5.6-py3-none-any.whl
fetch "https://files.pythonhosted.org/packages/54/74/418c1c0be49463799f9eeb307a8aa4013ff5fca5e0387f0ef2762fcdb4e2/$wheel" \
  f13865b00e42de5c2507c1889ba73c2bacd218a49d15b928c2a5c82667062ac5 "$cache/$wheel"
unzip -p "$cache/$wheel" ddddocr/common.onnx > "$scratch/common.onnx"
test "$(sha256 "$scratch/common.onnx")" = 33b5cd351ee94e73a6bf8fa18c415ed8b819b3ffd342e267c30d8ad8334e34e8
unzip -p "$cache/$wheel" ddddocr-1.5.6.dist-info/LICENSE > "$scratch/LICENSE-model.txt"

# Microsoft's CPU NuGet package contains the native runtime for all five targets.
package=microsoft.ml.onnxruntime
archive=$package.$version.nupkg
fetch "https://api.nuget.org/v3-flatcontainer/$package/$version/$archive" \
  25fe172f7fcdf34f2b5b02c8b997f6b88abc282956849d4af4335af23d0c4e4a "$cache/$archive"
native="runtimes/$platform/native"
libraries=("$library")
case "$target" in
  linux/*) libraries+=(libonnxruntime_providers_shared.so) ;;
  windows/*) libraries+=(onnxruntime_providers_shared.dll onnxruntime_providers_shared.lib) ;;
esac
for file in "${libraries[@]}"; do
  unzip -p "$cache/$archive" "$native/$file" > "$scratch/$file"
done
unzip -p "$cache/$archive" LICENSE > "$scratch/LICENSE-runtime.txt"
unzip -p "$cache/$archive" ThirdPartyNotices.txt > "$scratch/ThirdPartyNotices.txt"
printf 'ddddocr 1.5.6 (beta model)\nONNX Runtime %s (%s)\n' "$version" "$target" > "$scratch/VERSIONS"
# Renaming files keeps an already running process's mapped library intact.
for file in "$scratch"/*; do
  chmod 644 "$file"
  mv -f "$file" "$dest/"
done
echo ">> OCR model and runtime: $dest ($target)"
