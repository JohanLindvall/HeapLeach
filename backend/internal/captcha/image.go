// SPDX-License-Identifier: MIT

package captcha

import (
	"fmt"
	"image"
	"image/color"
	"math"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

type pixels struct {
	data                    []byte
	width, height, channels int
}

// Pillow's RGB-to-L conversion rounds these fixed-point coefficients. Resize
// the whole RGB image before converting it; the cropped view is already gray.
func gray(r, g, b byte) byte {
	return byte((19595*int(r) + 38470*int(g) + 7471*int(b) + 32768) >> 16)
}

func views(img image.Image) []pixels {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	rgb := pixels{make([]byte, w*h*3), w, h, 3}
	luminance := make([]byte, w*h)
	x0, y0, x1, y1 := w, h, 0, 0
	for y := range h {
		for x := range w {
			// Converting to RGB drops alpha, just as the original reader did.
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			i := y*w + x
			rgb.data[i*3], rgb.data[i*3+1], rgb.data[i*3+2] = c.R, c.G, c.B
			v := gray(c.R, c.G, c.B)
			luminance[i] = v
			if v != 255 {
				x0, y0, x1, y1 = min(x0, x), min(y0, y), max(x1, x+1), max(y1, y+1)
			}
		}
	}
	if x0 >= x1 || y0 >= y1 {
		return nil
	}
	crop := pixels{make([]byte, (x1-x0)*(y1-y0)), x1 - x0, y1 - y0, 1}
	lo, hi := byte(255), byte(0)
	for y := range crop.height {
		for x := range crop.width {
			v := luminance[(y+y0)*w+x+x0]
			crop.data[y*crop.width+x] = v
			lo, hi = min(lo, v), max(hi, v)
		}
	}
	if hi > lo {
		scale := 255.0 / float64(hi-lo)
		offset := -float64(lo) * scale
		for i, v := range crop.data {
			crop.data[i] = byte(min(255, max(0, int(float64(v)*scale+offset))))
		}
	}
	return []pixels{crop, rgb}
}

func (p pixels) tensor() ([]float32, int, error) {
	w := int(float64(p.width) * (float64(config.CaptchaModelHeight) / float64(p.height)))
	if w < 1 || w > config.CaptchaModelMaxWidth {
		return nil, 0, fmt.Errorf("CAPTCHA aspect ratio is too large or too small")
	}
	p = p.resize(w, config.CaptchaModelHeight)
	data := make([]float32, w*config.CaptchaModelHeight)
	for i := range data {
		v := p.data[i]
		if p.channels == 3 {
			v = gray(p.data[3*i], p.data[3*i+1], p.data[3*i+2])
		}
		// Keep the float32 rounding of NumPy's original normalization.
		value := float32(v) / 255
		data[i] = (value - 0.5) / 0.5
	}
	return data, w, nil
}

type filter struct {
	start   int
	weights []int32
}

func lanczos(x float64) float64 {
	if x == 0 {
		return 1
	}
	if x < -3 || x >= 3 {
		return 0
	}
	x *= math.Pi
	return (math.Sin(x) / x) * (math.Sin(x/3) / (x / 3))
}

// Pillow's separable Lanczos-3 resampler uses 22-bit coefficients and clips to
// bytes after each pass. Matching both matters for faint character outlines;
// another interpolation filter can change the model's preferred reading.
func filters(size, out int) []filter {
	const precision = 1 << 22
	scale := float64(size) / float64(out)
	filterScale := max(1, scale)
	support := 3 * filterScale
	result := make([]filter, out)
	for i := range out {
		center := (float64(i) + 0.5) * scale
		start := max(0, int(center-support+0.5))
		end := min(size, int(center+support+0.5))
		weights := make([]float64, end-start)
		total := 0.0
		for j := range weights {
			weights[j] = lanczos((float64(j+start) - center + 0.5) / filterScale)
			total += weights[j]
		}
		f := filter{start: start, weights: make([]int32, len(weights))}
		for j, weight := range weights {
			if total != 0 {
				weight /= total
			}
			f.weights[j] = int32(math.Round(weight * precision))
		}
		result[i] = f
	}
	return result
}

func (p pixels) resize(w, h int) pixels {
	if w != p.width {
		out := pixels{make([]byte, w*p.height*p.channels), w, p.height, p.channels}
		for x, f := range filters(p.width, w) {
			for y := range p.height {
				for c := range p.channels {
					sum := int64(1 << 21)
					for k, weight := range f.weights {
						sum += int64(p.data[(y*p.width+f.start+k)*p.channels+c]) * int64(weight)
					}
					out.data[(y*w+x)*p.channels+c] = byte(min(255, max(0, sum>>22)))
				}
			}
		}
		p = out
	}
	if h != p.height {
		out := pixels{make([]byte, w*h*p.channels), w, h, p.channels}
		for y, f := range filters(p.height, h) {
			for x := range w {
				for c := range p.channels {
					sum := int64(1 << 21)
					for k, weight := range f.weights {
						sum += int64(p.data[((f.start+k)*w+x)*p.channels+c]) * int64(weight)
					}
					out.data[(y*w+x)*p.channels+c] = byte(min(255, max(0, sum>>22)))
				}
			}
		}
		p = out
	}
	return p
}
