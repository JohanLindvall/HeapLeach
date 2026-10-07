# SPDX-License-Identifier: MIT

import unittest
from pathlib import Path
from unittest.mock import Mock

import ddddocr
from PIL import Image

from recognize import readings

# Enough of the model's alphabet for the decoder: the blank, both cases of
# every letter, the digits, and one symbol no answer can contain.
CHARSET = [""] + list("abcdefghijklmnopqrstuvwxyz0123456789") + list("ABCDEFGHIJKLMNOPQRSTUVWXYZ") + ["六"]
CLEAR = {"": 0.9}


def model_output(*steps):
    """What the model says, one {symbol: probability} dict per step; the rest
    of each step is spread thinly over every other symbol."""
    rows = []
    for step in steps:
        rest = (1 - sum(step.values())) / (len(CHARSET) - len(step))
        rows.append([step.get(c, rest) for c in CHARSET])
    return {"charsets": CHARSET, "probability": rows}


def spelled(text, last=None):
    """Steps spelling text clearly, each symbol followed by a blank."""
    steps = []
    for c in text:
        steps += [{c: 0.9}, CLEAR]
    return steps + ([last, CLEAR] if last else [])


def reader_saying(*steps):
    reader = Mock()
    reader.classification.return_value = model_output(*steps)
    return reader


IMAGE = Image.new("RGB", (100, 50))


class RecognitionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.reader = ddddocr.DdddOcr(show_ad=False, beta=True)

    def test_reads_outlined_characters_with_the_bundled_model(self):
        for filename, expected in (
            ("narrow-outlines.png", "m4qr7t"),
            ("faint-outlines.png", "b3xk9p"),
        ):
            with self.subTest(filename=filename):
                with Image.open(Path(__file__).parent / "testdata" / filename) as image:
                    self.assertEqual(readings(self.reader, image)[0], expected)

    def test_a_faint_character_still_makes_six(self):
        # Blank outweighs the last symbol, so the model's own string is five.
        reader = reader_saying(*spelled("ab3de", {"": 0.7, "7": 0.25}))
        self.assertEqual(readings(reader, IMAGE)[0], "ab3de7")

    def test_upper_and_lower_case_count_together(self):
        reader = reader_saying(*spelled("ab3de", {"K": 0.3, "k": 0.3, "x": 0.35}))
        self.assertEqual(readings(reader, IMAGE)[0], "ab3dek")

    def test_symbols_outside_the_alphabet_cannot_outvote_a_letter(self):
        reader = reader_saying(*spelled("ab3de", {"六": 0.6, "q": 0.3}))
        self.assertEqual(readings(reader, IMAGE)[0], "ab3deq")

    def test_a_doubled_symbol_needs_a_blank_between(self):
        apart = reader_saying({"a": 0.9}, CLEAR, *spelled("ab3de"))
        self.assertEqual(readings(apart, IMAGE)[0], "aab3de")
        together = reader_saying({"a": 0.9}, *spelled("ab3de7"))
        self.assertEqual(readings(together, IMAGE)[0], "ab3de7")

    def test_the_runner_up_follows(self):
        # A bar reads as either I or l; the next reading settles it.
        reader = reader_saying(*spelled("ab3de", {"I": 0.5, "l": 0.45}))
        self.assertEqual(readings(reader, IMAGE)[:2], ["ab3dei", "ab3del"])

    def test_blank_image_does_not_invent_an_answer(self):
        reader = Mock()
        self.assertEqual(readings(reader, Image.new("RGB", (100, 50), "white")), [])
        reader.classification.assert_not_called()


if __name__ == "__main__":
    unittest.main()
