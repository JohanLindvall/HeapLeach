#!/usr/bin/env bash
#
# Regenerates the pictures in docs/.
#
# Nothing is downloaded to make them. The queue they show is a fixture in
# backend/cmd/screenshots, served to a real browser through the real web UI
# and drawn for the terminal shot by the display's own renderer — so the
# pictures are of the program, not drawings of it, while what they show is
# invented.
#
# Needs: Chrome (or Chromium), Go, and a built frontend. Run `make screenshots`
# rather than this directly, which handles the frontend for you.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
docs="$root/docs"
port=${PORT:-8099}
url="http://127.0.0.1:$port"

chrome=""
for candidate in google-chrome chromium chromium-browser google-chrome-stable; do
  if command -v "$candidate" >/dev/null 2>&1; then chrome=$candidate; break; fi
done
if [ -z "$chrome" ]; then
  echo "no chrome or chromium on PATH; cannot capture screenshots" >&2
  exit 1
fi

work=$(mktemp -d)
cleanup() {
  [ -n "${server_pid:-}" ] && kill "$server_pid" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

shoot() { # shoot <output> <width> <height> <url>
  # Reduced motion is not a preference here, it is what makes the capture
  # deterministic. The job cards animate in from opacity 0, and the virtual
  # clock can land the screenshot mid-animation — which photographs a queue
  # of seven downloads as an empty page, convincingly enough to be mistaken
  # for a bug in the UI. The stylesheet already honours the setting, so this
  # asks for the settled state, which is what a picture should show anyway.
  "$chrome" --headless --disable-gpu --no-sandbox --hide-scrollbars \
    --force-prefers-reduced-motion \
    --user-data-dir="$work/chrome" \
    --window-size="$2,$3" --screenshot="$1" \
    --virtual-time-budget=4000 "$4" >/dev/null 2>&1
}

# --- the web UI ------------------------------------------------------------

# Refuse a port something else already holds. Chrome would otherwise happily
# photograph whatever that other thing serves, and the only clue would be the
# pictures looking wrong.
if curl -sf --max-time 2 "$url/api/state" >/dev/null 2>&1; then
  echo "something is already serving on $url; stop it or set PORT=" >&2
  exit 1
fi

( cd "$root/backend" && go run ./cmd/screenshots -addr "127.0.0.1:$port" ) &
server_pid=$!

for _ in $(seq 1 100); do
  if curl -sf "$url/api/state" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
curl -sf "$url/api/state" >/dev/null || { echo "the scene server never came up" >&2; exit 1; }

shoot "$docs/screenshot-dark.png"   1280 900 "$url/?theme=dark"
shoot "$docs/screenshot-light.png"  1280 900 "$url/?theme=light"
shoot "$docs/screenshot-mobile.png"  430 900 "$url/?theme=dark"
echo ">> web UI: dark, light, mobile"

# --- the terminal ----------------------------------------------------------

( cd "$root/backend" && go run ./cmd/screenshots -cli -width 96 ) > "$work/frame.ansi"
python3 "$docs/ansi-to-html.py" < "$work/frame.ansi" > "$work/terminal.html"
# Sized to the window rather than to a canvas it floats in: the frame is
# only as tall as the queue makes it.
shoot "$docs/screenshot-cli.png" 900 340 "file://$work/terminal.html"
echo ">> terminal"

echo ">> wrote $(ls "$docs"/screenshot-*.png | wc -l) pictures to docs/"
