#!/usr/bin/env bash
#
# Fetch one video with yt-dlp.
#
# The service shells out to this rather than driving yt-dlp inline, so the
# whole recipe — format selection, merging, retries — is in one readable
# place and can be adjusted without rebuilding. A copy placed next to the
# downd binary overrides the one built in.
#
# Usage: yt-download.sh <url> <output-dir> [format-selector]
#
# Environment:
#   YTDLP   path to yt-dlp                 (default: yt-dlp from PATH)
#   FFMPEG  path to ffmpeg                 (optional; enables merging)
#
# Lines the service reads back from stdout:
#   PROGRESS <downloaded-bytes> <total-bytes>
#   FILE <final-path>
set -euo pipefail

url=${1:?usage: yt-download.sh <url> <output-dir> [format]}
outdir=${2:?usage: yt-download.sh <url> <output-dir> [format]}
format=${3:-}

ytdlp=${YTDLP:-yt-dlp}
if ! command -v "$ytdlp" >/dev/null 2>&1; then
  echo "yt-dlp not found (looked for '$ytdlp')" >&2
  exit 127
fi

# Without ffmpeg only a single already-muxed stream can be saved, so ask for
# one rather than letting yt-dlp pick a pair it then cannot join.
if [ -z "$format" ]; then
  if [ -n "${FFMPEG:-}" ]; then
    format='bv*+ba/b'
  else
    format='b'
  fi
fi

args=(
  --no-warnings
  --no-playlist
  --newline
  # --print implies --quiet, which would hide progress; --progress restores it.
  --progress
  --no-part
  --no-mtime
  --retries 10
  --fragment-retries 10
  --concurrent-fragments 4
  --format "$format"
  --paths "$outdir"
  --output '%(title).200B [%(id)s].%(ext)s'
  --progress-template 'download:PROGRESS %(progress.downloaded_bytes)s %(progress.total_bytes,progress.total_bytes_estimate)s'
  --print 'after_move:FILE %(filepath)s'
)
if [ -n "${FFMPEG:-}" ]; then
  args+=(--ffmpeg-location "$FFMPEG")
fi

exec "$ytdlp" "${args[@]}" "$url"
