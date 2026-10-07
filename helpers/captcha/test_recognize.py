# SPDX-License-Identifier: MIT

import unittest
from pathlib import Path
from unittest.mock import Mock

import ddddocr
from PIL import Image

from recognize import recognize


class RecognitionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.reader = ddddocr.DdddOcr(show_ad=False, beta=True)

    def test_recovers_outlined_characters_with_the_bundled_model(self):
        for filename, expected in (
            ("narrow-outlines.png", "m4qr7t"),
            ("faint-outlines.png", "b3xk9p"),
        ):
            with self.subTest(filename=filename):
                with Image.open(Path(__file__).parent / "testdata" / filename) as image:
                    self.assertEqual(recognize(self.reader, image), expected)

    def test_keeps_a_complete_answer_and_its_case(self):
        reader = Mock()
        reader.classification.return_value = "aB3dE7"
        self.assertEqual(recognize(reader, Image.new("RGB", (100, 50))), "aB3dE7")
        reader.classification.assert_called_once()

    def test_invalid_answers_try_another_view(self):
        reader = Mock()
        reader.classification.side_effect = ["六六六六六六", "aB3dE!", "aB3dE7"]
        self.assertEqual(recognize(reader, Image.new("RGB", (100, 50))), "aB3dE7")

    def test_exhausted_views_return_no_answer(self):
        reader = Mock()
        reader.classification.return_value = "aB3dE"
        self.assertEqual(recognize(reader, Image.new("RGB", (100, 50))), "")

    def test_blank_image_does_not_invent_an_answer(self):
        reader = Mock()
        self.assertEqual(recognize(reader, Image.new("RGB", (100, 50), "white")), "")
        reader.classification.assert_not_called()


if __name__ == "__main__":
    unittest.main()
