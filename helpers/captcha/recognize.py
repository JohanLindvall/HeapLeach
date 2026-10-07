# SPDX-License-Identifier: MIT
"""Local image CAPTCHA reader: image bytes on stdin, recognized text on stdout.

The caller bounds image size, runtime and retries. No network service is used.
The beta model is selected deliberately: its predecessor regularly misreads
the outlined, coloured characters in Keep2Share's challenges.
"""

import io
import sys

import ddddocr
from PIL import Image, ImageOps


def complete(answer):
    return len(answer) == 6 and answer.isascii() and answer.isalnum()


def recognize(reader, image):
    image = image.convert("RGB")
    bounds = ImageOps.invert(image.convert("L")).getbbox()
    if bounds is None:
        return ""
    crop = image.crop(bounds)
    for variant in (crop, image):
        answer = reader.classification(variant)
        if complete(answer):
            return answer

    # Pale outlines can disappear when the model rescales to 64 pixels
    # high. Contrast recovers them; a white border keeps edge characters
    # away from the boundary. Wider views separate overlapping characters.
    # Retry the same challenge locally before spending another host attempt.
    contrast = ImageOps.autocontrast(crop.convert("L"))
    border = max(1, round(contrast.height / 10))
    for variant in (
        contrast,
        ImageOps.expand(contrast, border=border, fill=255),
        contrast.resize((round(contrast.width * 1.25), contrast.height), Image.Resampling.LANCZOS),
        contrast.resize((round(contrast.width * 1.5), contrast.height), Image.Resampling.LANCZOS),
    ):
        answer = reader.classification(variant)
        if complete(answer):
            return answer
    return ""


if __name__ == "__main__":
    reader = ddddocr.DdddOcr(show_ad=False, beta=True)
    image = Image.open(io.BytesIO(sys.stdin.buffer.read()))
    print(recognize(reader, image))
