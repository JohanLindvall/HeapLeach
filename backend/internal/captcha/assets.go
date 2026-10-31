// SPDX-License-Identifier: MIT

package captcha

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

const modelSHA256 = "33b5cd351ee94e73a6bf8fa18c415ed8b819b3ffd342e267c30d8ad8334e34e8"

func assetPaths() (model, library string, err error) {
	dir := config.LookupEnv("OCR_DIR")
	if dir == "" {
		exe, err := util.ExecutableDir()
		if err != nil {
			return "", "", err
		}
		dir = filepath.Join(exe, "ocr")
	}
	lib := "libonnxruntime.so"
	switch runtime.GOOS {
	case "darwin":
		lib = "libonnxruntime.dylib"
	case "windows":
		lib = "onnxruntime.dll"
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	model, library = filepath.Join(dir, "common.onnx"), filepath.Join(dir, lib)
	for _, path := range []string{model, library} {
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return "", "", fmt.Errorf("captcha: local OCR data is missing from %s; keep the release's ocr directory beside heapleach, or run `make ocr-assets`", dir)
		}
	}
	return model, library, nil
}
