// SPDX-License-Identifier: MIT

//go:build cgo

package captcha

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	ort "github.com/yalue/onnxruntime_go"
)

// The runtime is process-wide. Initialization failures remain retryable after
// the user installs the assets; initialized native state is kept until exit.
var runtimeMu sync.Mutex

// Available checks the installation before the caller requests a challenge.
func Available() error {
	_, library, err := assetPaths()
	if err != nil {
		return err
	}
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if ort.IsInitialized() {
		return nil
	}
	ort.SetSharedLibraryPath(library)
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("captcha: initialize ONNX Runtime: %w", err)
	}
	return nil
}

type nativeEngine struct{ session *ort.DynamicAdvancedSession }

func newEngine() (engine, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	model, _, err := assetPaths()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(model)
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(data)) != modelSHA256 {
		return nil, fmt.Errorf("OCR model checksum does not match; run `make ocr-assets` to restore the pinned model")
	}
	options, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer options.Destroy()
	if err := options.SetIntraOpNumThreads(config.CaptchaThreads); err != nil {
		return nil, err
	}
	if err := options.SetInterOpNumThreads(1); err != nil {
		return nil, err
	}
	if err := options.AddSessionConfigEntry("session.intra_op.allow_spinning", "0"); err != nil {
		return nil, err
	}
	session, err := ort.NewDynamicAdvancedSessionWithONNXData(data, []string{"input1"}, []string{"387"}, options)
	if err != nil {
		return nil, err
	}
	return &nativeEngine{session}, nil
}

func (r *nativeEngine) close() { _ = r.session.Destroy() }

func (r *nativeEngine) infer(ctx context.Context, data []float32, width int) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input, err := ort.NewTensor(ort.NewShape(1, 1, config.CaptchaModelHeight, int64(width)), data)
	if err != nil {
		return nil, err
	}
	defer input.Destroy()
	options, err := ort.NewRunOptions()
	if err != nil {
		return nil, err
	}
	defer options.Destroy()
	terminated := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = options.Terminate()
		close(terminated)
	})
	defer func() {
		// Join a concurrent Terminate before releasing its RunOptions.
		if !stop() {
			<-terminated
		}
	}()
	output := []ort.Value{nil}
	defer func() {
		for _, value := range output {
			if value != nil {
				_ = value.Destroy()
			}
		}
	}()
	err = r.session.RunWithOptions([]ort.Value{input}, output, options)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	tensor, ok := output[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("unexpected OCR output type %T", output[0])
	}
	shape := tensor.GetShape()
	if len(shape) != 3 || shape[1] != 1 || shape[2] != config.CaptchaModelAlphabet {
		return nil, fmt.Errorf("unexpected OCR output shape %v", shape)
	}
	return append([]float32(nil), tensor.GetData()...), nil
}
