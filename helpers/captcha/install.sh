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
  linux/amd64) platform=linux-x64; checksum=1fa4dcaef22f6f7d5cd81b28c2800414350c10116f5fdd46a2160082551c5f9b; library=libonnxruntime.so ;;
  linux/arm64) platform=linux-aarch64; checksum=7c63c73560ed76b1fac6cff8204ffe34fe180e70d6582b5332ec094810241e5c; library=libonnxruntime.so ;;
  darwin/amd64) platform=osx-x86_64; checksum=d10359e16347b57d9959f7e80a225a5b4a66ed7d7e007274a15cae86836485a6; library=libonnxruntime.dylib ;;
  darwin/arm64) platform=osx-arm64; checksum=b4d513ab2b26f088c66891dbbc1408166708773d7cc4163de7bdca0e9bbb7856; library=libonnxruntime.dylib ;;
  windows/amd64) platform=win-x64; checksum=0b38df9af21834e41e73d602d90db5cb06dbd1ca618948b8f1d66d607ac9f3cd; library=onnxruntime.dll ;;
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

folder=onnxruntime-$platform-$version
extension=tgz
if [ "${target%%/*}" = windows ]; then extension=zip; fi
archive=$folder.$extension
fetch "https://github.com/microsoft/onnxruntime/releases/download/v$version/$archive" "$checksum" "$cache/$archive"
if [ "$extension" = zip ]; then
  unzip -q "$cache/$archive" -d "$scratch"
else
  tar -xzf "$cache/$archive" -C "$scratch"
fi
cp -L "$scratch/$folder/lib/$library" "$scratch/$library"
cp "$scratch/$folder/LICENSE" "$scratch/LICENSE-runtime.txt"
cp "$scratch/$folder/ThirdPartyNotices.txt" "$scratch/ThirdPartyNotices.txt"
# Some CPU archives include a provider-support library. Preserve it when present.
for support in "$scratch/$folder"/lib/*providers_shared*; do
  if [ -f "$support" ]; then cp -L "$support" "$scratch/"; fi
done
rm -rf "${scratch:?}/${folder:?}"
printf 'ddddocr 1.5.6 (beta model)\nONNX Runtime %s (%s)\n' "$version" "$target" > "$scratch/VERSIONS"
# Renaming files keeps an already running process's mapped library intact.
for file in "$scratch"/*; do
  chmod 644 "$file"
  mv -f "$file" "$dest/"
done
echo ">> OCR model and runtime: $dest ($target)"
