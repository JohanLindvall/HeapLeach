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
#   DENO    path to a deno binary          (optional; runs the player JS)
#
# Lines the service reads back from stdout:
#   PROGRESS <downloaded-bytes> <total-bytes> [<format-id>]
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


# best_audio_per_language builds the format selector, keeping every language
# a video offers rather than only the one yt-dlp would rank first.
#
# A dubbed release is the case this exists for: the default `ba` takes the
# single best audio, which for a video whose original track is Spanish means
# the English dub beside it is silently dropped. Asking for each language by
# name keeps them all, and the player picks.
#
# Naming them is necessary because `mergeall[vcodec=none]` — the documented
# way to take "all audio" — takes all *formats*, which is every bitrate and
# codec of every language: the video this was written against offers 28 audio
# formats for its 4 languages, so that would fetch seven copies of each.
#
# The languages have to be asked for, which costs one extraction before the
# download. Any failure falls back to the single-audio selector, because a
# download that works with one language beats no download at all.
best_audio_per_language() {
  local langs selector lang
  langs=$("$ytdlp" "${probe_args[@]}" --print '%(formats.:.language)s' "$url" 2>/dev/null |
    tr -d "[]'\"" | tr ',' '\n' |
    sed 's/^[[:space:]]*//; s/[[:space:]]*$//' |
    grep -E '^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$' |
    sort -u) || true

  selector=''
  for lang in $langs; do
    selector="$selector+ba[language=$lang]"
  done

  # One language needs no naming, and none means the probe told us nothing.
  if [ -z "$selector" ] || [ "$(printf '%s\n' $langs | wc -l)" -lt 2 ]; then
    echo 'bv*+ba/b'
    return
  fi
  # The plain pair trails it, so a video whose languages cannot all be had
  # still downloads rather than failing the selector outright.
  echo "bv*$selector/bv*+ba/b"
}

# probe_args are the flags the language probe shares with the download: the
# runtime and the player it needs, and nothing that would fetch bytes.
probe_args=(--no-warnings --no-playlist --simulate)
if [ -n "${DENO:-}" ]; then
  probe_args+=(--js-runtimes "deno:$DENO")
fi

# Chosen here rather than earlier because the probe above is what decides it.
# Without ffmpeg only a single already-muxed stream can be saved, so ask for
# one rather than letting yt-dlp pick a set it then cannot join.
if [ -z "$format" ]; then
  if [ -n "${FFMPEG:-}" ]; then
    format=$(best_audio_per_language)
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
  # Without this yt-dlp keeps one audio stream however many the selector
  # asked for, and the extra languages are downloaded and then discarded.
  --audio-multistreams
  --paths "$outdir"
  --output '%(title).200B [%(id)s].%(ext)s'
  # The format id comes last and is what marks one stream from the next: a
  # merged download reports the video's progress and then the audio's, each
  # counting from zero, so without it the item ends up showing the audio
  # stream's few megabytes as the whole file.
  --progress-template 'download:PROGRESS %(progress.downloaded_bytes)s %(progress.total_bytes,progress.total_bytes_estimate)s %(info.format_id)s'
  --print 'after_move:FILE %(filepath)s'
)
if [ -n "${FFMPEG:-}" ]; then
  args+=(--ffmpeg-location "$FFMPEG")
fi
# Merging several languages together loses what each one was: ffmpeg labels
# every track with the first one's tag, so a file with Arabic, English,
# Spanish and Portuguese arrives claiming to be English four times over, and
# a player has no way to offer the choice the tracks were fetched for. The
# metadata pass is what restores them — at the cost of a second pass over the
# finished file, so it is asked for only when there are languages to label.
case "$format" in
*language=*) args+=(--embed-metadata) ;;
esac
# yt-dlp looks for deno on PATH unaided; this is for the copy next to the
# binary, which is where the service keeps its helpers and PATH does not
# reach. Without a runtime it still extracts, for now, but says it is
# deprecated and that formats may be missing.
if [ -n "${DENO:-}" ]; then
  args+=(--js-runtimes "deno:$DENO")
fi

exec "$ytdlp" "${args[@]}" "$url"
