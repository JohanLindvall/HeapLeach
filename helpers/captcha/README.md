# Local CAPTCHA reader

`heapleach-ocr` reads an image from standard input and writes recognized text
to standard output. HeapLeach validates the answer and retries rejected
CAPTCHAs. It also bounds image size and runtime and terminates the helper on
cancellation. The helper uses CPU inference and makes no network calls.

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
