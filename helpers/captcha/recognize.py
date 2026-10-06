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


if __name__ == "__main__":
    reader = ddddocr.DdddOcr(show_ad=False, beta=True)
    image = Image.open(io.BytesIO(sys.stdin.buffer.read())).convert("RGB")
    # The model rescales the whole image to a fixed height. Removing white
    # margins gives thin outlines more pixels at that height. Keep the
    # original as a fallback when cropping produces an incomplete answer.
    bounds = ImageOps.invert(image.convert("L")).getbbox()
    answer = reader.classification(image.crop(bounds)) if bounds else ""
    if len(answer) != 6 or not answer.isascii() or not answer.isalnum():
        answer = reader.classification(image)
    print(answer)
