These CAPTCHA images are synthetic. Neither came from a host. They exercise
the real bundled model: both the old cropped view and the original image
lose characters, while contrast and spacing recover the complete answer.

They were generated with Pillow 12.3.0 and Liberation Serif Regular:

```python
from PIL import Image, ImageDraw, ImageFont

font = ImageFont.truetype("LiberationSerif-Regular.ttf", 70)
for name, text, shade, width in (
    ("narrow-outlines", "m4qr7t", 200, 180),
    ("faint-outlines", "b3xk9p", 250, 210),
):
    image = Image.new("RGB", (280, 100), "white")
    ImageDraw.Draw(image).text(
        (10, 10), text, font=font, fill="white", stroke_width=1,
        stroke_fill=(shade, shade, shade),
    )
    image.resize((width, 100), Image.Resampling.LANCZOS).save(name + ".png")
```

Tests read the PNGs, so they do not need a font installed.
