// SPDX-License-Identifier: MIT

package captcha

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
)

type reference struct {
	Tensors []struct {
		Width  int
		SHA256 string
	}
	Readings []string
}

func references(t *testing.T) map[string]reference {
	t.Helper()
	raw, err := os.ReadFile("testdata/reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var refs map[string]reference
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatal(err)
	}
	return refs
}

// These hashes were produced by the Python reader's Pillow/NumPy pipeline on
// the synthetic fixtures. They check byte-for-byte preprocessing parity, not
// just that the resulting images look similar to a human.
func TestPreprocessingMatchesPython(t *testing.T) {
	for name, ref := range references(t) {
		t.Run(name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			img, _, err := image.Decode(f)
			if err != nil {
				t.Fatal(err)
			}
			for i, view := range views(img) {
				tensor, width, err := view.tensor()
				if err != nil {
					t.Fatal(err)
				}
				data := make([]byte, 4*len(tensor))
				for j, v := range tensor {
					binary.LittleEndian.PutUint32(data[j*4:], math.Float32bits(v))
				}
				got := fmt.Sprintf("%x", sha256.Sum256(data))
				if width != ref.Tensors[i].Width || got != ref.Tensors[i].SHA256 {
					t.Errorf("view %d: width %d, sha256 %s; want %d, %s", i, width, got, ref.Tensors[i].Width, ref.Tensors[i].SHA256)
				}
			}
		})
	}
}

func TestBlankImageHasNoViews(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 100, 50))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	if got := views(img); len(got) != 0 {
		t.Fatal("blank image produced model inputs")
	}
}

func TestViewBoundsAndAspectRatio(t *testing.T) {
	img := image.NewNRGBA(image.Rect(4, 5, 104, 55))
	for y := 5; y < 55; y++ {
		for x := 4; x < 104; x++ {
			img.SetNRGBA(x, y, color.NRGBA{255, 255, 255, 255})
		}
	}
	img.SetNRGBA(10, 20, color.NRGBA{100, 100, 100, 255})
	v := views(img)
	if len(v) != 2 || v[0].width != 1 || v[0].height != 1 || v[0].data[0] != 100 {
		t.Fatalf("incorrect crop: %v", v)
	}
	for _, shape := range [][2]int{{10000, 1}, {1, 10000}} {
		p := pixels{width: shape[0], height: shape[1], channels: 1}
		if _, _, err := p.tensor(); err == nil {
			t.Fatalf("accepted pathological aspect ratio %v", shape)
		}
	}
}
