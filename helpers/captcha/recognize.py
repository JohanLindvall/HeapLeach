# SPDX-License-Identifier: MIT
"""Local image CAPTCHA reader: image bytes on stdin, ranked readings on stdout.

Keep2Share's challenges are six letters and digits, and the site compares
answers without regard to case. The model's own best string regularly
drops a pale or hairline character and comes back short, so its per-step
probabilities are decoded under those rules instead: exactly six symbols,
with upper and lower case pooled. The image is read twice, and readings
are ranked by their likelihood under both views.

One reading per line, most likely first. A wrong answer does not spend the
challenge, so the caller can try the next; it bounds how many, as well as
image size, runtime and retries. No network service is used.

The beta model is selected deliberately: its predecessor regularly misreads
the outlined, coloured characters in Keep2Share's challenges.
"""

import io
import math
import sys

import ddddocr
import numpy as np
from PIL import Image, ImageOps

LENGTH = 6
SYMBOLS = "abcdefghijklmnopqrstuvwxyz0123456789"
READINGS = 5  # returned
BEAM = 32  # prefixes kept at each step of the search
PROPOSALS = 8  # readings each view puts forward for the joint ranking
FLOOR = -9.0  # a symbol less likely than this at a step is not extended
NONE = float("-inf")


def add(a, b):
    """Sum two log-probabilities."""
    if a == NONE:
        return b
    if b == NONE:
        return a
    hi, lo = max(a, b), min(a, b)
    return hi + math.log1p(math.exp(lo - hi))


def log_probs(reader, view):
    """Per-step log-probabilities over the blank and SYMBOLS, case pooled.

    The model's alphabet holds thousands of other symbols; their share is
    dropped rather than allowed to outvote a letter, since no answer can
    contain one.
    """
    out = reader.classification(view, probability=True)
    index = {c: i for i, c in enumerate(out["charsets"])}
    p = np.asarray(out["probability"], dtype=np.float64)
    if p.ndim != 2 or not np.isfinite(p).all():
        return None
    columns = [p[:, index[""]]]
    for c in SYMBOLS:
        column = p[:, index[c]]
        if c.upper() != c:
            column = column + p[:, index[c.upper()]]
        columns.append(column)
    q = np.stack(columns, axis=1)
    q /= np.maximum(q.sum(axis=1, keepdims=True), np.finfo(q.dtype).tiny)
    with np.errstate(divide="ignore"):
        return np.log(q).tolist()


def proposals(table):
    """CTC prefix beam search for readings of exactly LENGTH symbols."""
    # prefix -> (log-probability ending in a blank, ending in its last symbol)
    beams = {(): (0.0, NONE)}
    for row in table:
        symbols = [k for k in range(1, len(row)) if row[k] > FLOOR]
        grown = {}

        def extend(prefix, blank, symbol):
            b, s = grown.get(prefix, (NONE, NONE))
            grown[prefix] = (add(b, blank), add(s, symbol))

        for prefix, (b, s) in beams.items():
            total = add(b, s)
            extend(prefix, total + row[0], NONE)
            for k in symbols:
                if prefix and prefix[-1] == k:
                    # Only a blank in between makes a repeat a second symbol.
                    extend(prefix, NONE, s + row[k])
                    if len(prefix) < LENGTH:
                        extend(prefix + (k,), NONE, b + row[k])
                elif len(prefix) < LENGTH:
                    extend(prefix + (k,), NONE, total + row[k])
        beams = dict(sorted(grown.items(), key=lambda kv: -add(*kv[1]))[:BEAM])
    done = sorted((-add(*v), p) for p, v in beams.items() if len(p) == LENGTH)
    return ["".join(SYMBOLS[k - 1] for k in p) for _, p in done[:PROPOSALS]]


def likelihood(table, reading):
    """Log-probability of a reading over all of its alignments (CTC forward)."""
    labels = [0]
    for c in reading:
        labels += [SYMBOLS.index(c) + 1, 0]
    alpha = [NONE] * len(labels)
    alpha[0], alpha[1] = table[0][0], table[0][labels[1]]
    for row in table[1:]:
        prev, alpha = alpha, [NONE] * len(labels)
        for i, k in enumerate(labels):
            a = add(prev[i], prev[i - 1]) if i else prev[i]
            if i > 1 and k and k != labels[i - 2]:
                a = add(a, prev[i - 2])
            alpha[i] = a + row[k]
    return add(alpha[-1], alpha[-2])


def readings(reader, image):
    """Readings of a challenge image, most likely first."""
    image = image.convert("RGB")
    bounds = ImageOps.invert(image.convert("L")).getbbox()
    if bounds is None:
        return []
    # The model rescales each view to a fixed height. Cropping the white
    # margin gives thin outlines more pixels there, and stretching the
    # contrast makes a faint outline as dark as a strong one. The whole
    # image, at the other scale, errs differently, and the two together
    # misread less than either does alone.
    views = [ImageOps.autocontrast(image.crop(bounds).convert("L")), image]
    tables = [t for t in (log_probs(reader, v) for v in views) if t]
    found = {r for t in tables for r in proposals(t)}
    ranked = sorted((-sum(likelihood(t, r) for t in tables), r) for r in found)
    return [r for _, r in ranked[:READINGS]]


if __name__ == "__main__":
    reader = ddddocr.DdddOcr(show_ad=False, beta=True)
    for reading in readings(reader, Image.open(io.BytesIO(sys.stdin.buffer.read()))):
        print(reading)
