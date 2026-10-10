# Local CAPTCHA reader

`heapleach-ocr` reads an image from standard input and writes its readings
to standard output, one per line, most likely first. HeapLeach validates
each reading and tries up to three on the same challenge, since a wrong
answer does not spend it, before requesting another image. It also bounds
image size and runtime and terminates the helper on cancellation. It runs
at most two helpers at a time, since each loads the model and uses every core
it can. Each gets a `TMPDIR` of its own, which the frozen executable unpacks
its runtime into and which is removed after it exits, however it exits. The
helper uses CPU inference and makes no network calls.

Keep2Share's challenges are six letters and digits, and the site compares
answers without regard to case. The model's own best string regularly
drops a pale or hairline character and comes back five long, so the
reader decodes the model's per-step probabilities under those two rules
instead: exactly six symbols, with each letter's upper and lower case
pooled. It reads two views, the image cropped to its text with its
contrast stretched and the image whole, and ranks the readings by their
likelihood under both. On 66 live challenges, labelled by hand, the
model's own string was right for 41; retrying the unreadable ones with
more contrast, a border or wider spacing raised that to 42. The first
ranked reading is right for 44 to 50 of them, depending on which of I and
l each bar really is, and one of the first three for 60 either way. The
font draws a capital I and a lowercase l alike, so those readings rank
side by side and trying the next one settles it.

`make test-captcha` runs the reader against synthetic outlined challenges
using the actual model, and the decoding against hand-made model output.
It runs in Docker and is also part of `make check`, CI and helper builds.

From the repository root, `make captcha-helper` uses Docker to put a frozen
Linux executable in `bin/`, beside HeapLeach. It includes Python and the
model, so Python need not be installed on the machine running HeapLeach.
The helper needs glibc 2.36 or newer. The application remains a static Go
binary, and hosts that do not need OCR do not require this helper.

For a native build on another platform, create a Python 3.12 virtual
environment, activate it, and run these commands from this directory:

```sh
python -m pip install -r requirements.txt
python -m PyInstaller --noconfirm --onefile --name heapleach-ocr --collect-data ddddocr --collect-binaries onnxruntime --recursive-copy-metadata ddddocr recognize.py
```

Copy `dist/heapleach-ocr` (or `dist/heapleach-ocr.exe` on Windows) beside the
HeapLeach executable. PyInstaller builds for the platform it runs on; the
Docker recipe produces Linux binaries only. Native builds are not part of
the application's release archives.

The beta recognition model in [ddddocr 1.5.6](https://github.com/sml2h3/ddddocr)
(`common.onnx`) handles Keep2Share's outlined text better than the older
model. The Docker build includes only that model. Package versions are
pinned in `requirements.txt`, and their distribution metadata, including
the upstream license notices, is included in the executable with
`--recursive-copy-metadata`. ddddocr is MIT licensed; ONNX Runtime, NumPy,
Pillow and OpenCV retain their own licenses. No model files are stored in
this repository.
