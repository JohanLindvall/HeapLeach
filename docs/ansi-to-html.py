#!/usr/bin/env python3
"""Renders a captured terminal frame as an HTML page, for screenshotting.

The frame arrives on stdin exactly as the display writes it, so this only has
to understand the handful of SGR codes the display actually uses (see the
colour table in internal/cli/render.go). Anything else is dropped rather than
guessed at: an unrecognised escape has no business becoming visible text.
"""

import html
import re
import sys

# What the display paints with, mapped onto the palette the web UI uses so
# the two pictures look like one program.
COLOURS = {
    "31": "#ff6b6b",  # red
    "32": "#4ade80",  # green
    "33": "#fbbf24",  # yellow
    "35": "#c084fc",  # magenta
    "36": "#22d3ee",  # cyan
}

ESCAPE = re.compile(r"\x1b\[([0-9;?]*)([a-zA-Z])")

TITLE = "heapleach ~/Downloads"


def convert(text: str) -> str:
    """Turns one frame of ANSI into span-wrapped HTML."""
    out: list[str] = []
    open_spans = 0
    pos = 0

    def close_all() -> None:
        nonlocal open_spans
        out.append("</span>" * open_spans)
        open_spans = 0

    for match in ESCAPE.finditer(text):
        out.append(html.escape(text[pos:match.start()]))
        pos = match.end()
        if match.group(2) != "m":
            continue  # cursor moves and line clears leave no mark on a still
        for code in (match.group(1) or "0").split(";"):
            if code in ("0", "39", "22"):
                close_all()
            elif code == "1":
                out.append('<span style="font-weight:700">')
                open_spans += 1
            elif code == "2":
                out.append('<span style="opacity:.55">')
                open_spans += 1
            elif code in COLOURS:
                out.append(f'<span style="color:{COLOURS[code]}">')
                open_spans += 1

    out.append(html.escape(text[pos:]))
    close_all()
    return "".join(out)


def page(body: str) -> str:
    return f"""<!doctype html>
<html><head><meta charset="utf-8"><style>
  html, body {{ margin: 0; height: 100%; background: #0b0d17; }}
  body {{ display: flex; align-items: center; justify-content: center; padding: 22px;
          box-sizing: border-box; }}
  .window {{ width: 100%; background: #11131f; border-radius: 12px; overflow: hidden;
             border: 1px solid #232637; box-shadow: 0 18px 48px rgba(0,0,0,.55); }}
  .bar {{ display: flex; align-items: center; gap: 8px; padding: 11px 14px;
          background: #171a29; border-bottom: 1px solid #232637; }}
  .dot {{ width: 11px; height: 11px; border-radius: 50%; }}
  .title {{ margin-left: 8px; color: #8b90a7; font: 500 12.5px ui-sans-serif, system-ui, sans-serif; }}
  pre {{ margin: 0; padding: 18px 20px 20px; color: #d7dbe8; white-space: pre;
         font: 13px/1.55 ui-monospace, "SF Mono", "JetBrains Mono", Menlo, monospace; }}
</style></head>
<body>
  <div class="window">
    <div class="bar">
      <span class="dot" style="background:#ff5f57"></span>
      <span class="dot" style="background:#febc2e"></span>
      <span class="dot" style="background:#28c840"></span>
      <span class="title">{html.escape(TITLE)}</span>
    </div>
    <pre>{body}</pre>
  </div>
</body></html>
"""


def main() -> None:
    sys.stdout.write(page(convert(sys.stdin.read().rstrip("\n"))))


if __name__ == "__main__":
    main()
