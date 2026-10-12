# SPDX-License-Identifier: MIT

import io
import unittest
from pathlib import Path
from unittest.mock import Mock

import onnxruntime
from PIL import Image

from recognize import load, readings, serve

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
        session = onnxruntime.InferenceSession
        cls.reader = load()
        cls.restored = onnxruntime.InferenceSession is session

    def test_loading_leaves_onnx_runtime_as_it_found_it(self):
        self.assertTrue(self.restored)

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


def framed(*images):
    """Images as --serve reads them: each one's length, then its bytes."""
    return io.BytesIO(b"".join(b"%d\n" % len(raw) + raw for raw in images))


def encoded(image):
    out = io.BytesIO()
    image.save(out, "PNG")
    return out.getvalue()


class ServeTests(unittest.TestCase):
    def test_answers_one_image_after_another(self):
        reader = reader_saying(*spelled("ab3de7"))
        sink = io.BytesIO()
        serve(reader, framed(encoded(IMAGE), encoded(IMAGE)), sink)
        expected = readings(reader, IMAGE)
        self.assertEqual(expected[0], "ab3de7")
        answer = "".join(r + "\n" for r in expected) + "\n"
        self.assertEqual(sink.getvalue().decode(), "ready\n" + answer * 2)

    def test_an_image_without_text_is_answered_with_no_readings(self):
        sink = io.BytesIO()
        serve(Mock(), framed(encoded(Image.new("RGB", (100, 50), "white"))), sink)
        self.assertEqual(sink.getvalue(), b"ready\n\n")

    def test_input_that_ends_inside_an_image_ends_the_reader(self):
        sink = io.BytesIO()
        serve(Mock(), io.BytesIO(b"100\n" + encoded(IMAGE)[:10]), sink)
        self.assertEqual(sink.getvalue(), b"ready\n")

    def test_a_length_that_cannot_be_an_image_is_refused(self):
        for line in (b"0\n", b"-1\n", b"99999999999\n", b"many\n"):
            with self.subTest(line=line), self.assertRaises(ValueError):
                serve(Mock(), io.BytesIO(line), io.BytesIO())


if __name__ == "__main__":
    unittest.main()
