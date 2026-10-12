# Local CAPTCHA recognition

OCR runs inside HeapLeach through Go's `onnxruntime_go` binding. There is no
Python interpreter, PyInstaller archive, subprocess protocol or runtime
extraction. The model and native CPU library live in `ocr/` beside the
executable, or in the directory selected by `HEAPLEACH_OCR_DIR`.

`make build` exports the application and `bin/ocr`. `make native` and
`make dependencies` install the same data using `make ocr-assets`.
`make captcha-helper` remains an alias for existing scripts. Release archives
include `ocr/`, and the Docker image includes the entire model, native runtime
and required system libraries. Container startup does not download OCR data.

`install.sh` downloads ddddocr 1.5.6's wheel as an ordinary ZIP, extracts only
its beta model (`common.onnx`) and license, and downloads ONNX Runtime 1.23.2's
CPU archive for the target platform. Every download and the extracted model
are checked against pinned SHA-256 digests. No Python installation is required.
Downloads are cached under `~/.cache/heapleach/ocr` (or `HEAPLEACH_OCR_CACHE`).
The model is about 52 MiB. No model or native binaries are tracked in Git.

The runtime version is pinned deliberately: it publishes binaries for all five
release targets, including Intel macOS. The Go binding uses that runtime's C
API. Updating it requires updating the checksums and re-running the model tests
on each platform. Native builds need Go and a C compiler; asset installation
needs Bash, curl, unzip, tar and sha256sum (or shasum on macOS). Windows source
builds use a MinGW C compiler and Git Bash. The Windows native library also
requires Microsoft's Visual C++ runtime to be installed on the system.

The reader uses the existing model and six-character, case-insensitive CTC
beam search. It combines a cropped, contrast-stretched view with the whole
image, ranking their joint likelihoods. Go's image pipeline matches the old
Pillow/NumPy tensors byte for byte on the synthetic outlined challenges.
The tests also pin all five ranked readings, faint characters, repeated symbols,
case pooling and ambiguous alternatives. Fixtures and expected tensors are
synthetic; captured live challenges do not belong in the repository.

At most two sessions read images concurrently, with two inference threads each
and spinning disabled. Sessions are reused until five minutes idle. Waiting is
cancelable; the 20-second budget begins after admission. Cancellation terminates
inference. Native session construction cannot be interrupted, so a cancelled
caller's worker retains its slot until construction ends and frees that session.

`make test-captcha` runs the Go tests with the actual model and runtime, using
Docker if Go is absent. Ordinary `go test ./...` exercises preprocessing,
decoding and session ownership without downloading anything; model integration
tests require `HEAPLEACH_OCR_DIR`. CI extracts each release archive and runs
those model tests on its native Linux, macOS or Windows runner.

The installation includes ddddocr's MIT license and ONNX Runtime's license and
third-party notices. The preprocessing reference hashes and ranked readings in
`backend/internal/captcha/testdata/reference.json` were generated using the
previous `helpers/captcha/recognize.py` implementation, preserved in Git history.
