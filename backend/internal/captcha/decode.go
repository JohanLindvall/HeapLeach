// SPDX-License-Identifier: MIT

// Package captcha reads six-character image challenges locally. The trained
// model supplies character scores; the decoder combines two views of the image
// and enforces the challenge's alphabet and length.
package captcha

import (
	"fmt"
	"math"
	"sort"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

const symbols = "abcdefghijklmnopqrstuvwxyz0123456789"

// Columns in ddddocr 1.5.6's beta model (common.onnx). Keep the model checksum
// and these indices together when upgrading: the other 8,147 characters cannot
// occur in these challenges. Zero is the CTC blank.
var columns = [][2]int{
	{0, 0},
	{7266, 5461}, {4393, 1204}, {1151, 7961}, {6810, 3128}, {5428, 2547},
	{55, 311}, {5690, 598}, {7412, 1965}, {782, 6554}, {4730, 7216},
	{5737, 2113}, {2457, 210}, {320, 7844}, {1503, 901}, {4429, 3157},
	{209, 2341}, {7576, 1849}, {2376, 5629}, {2714, 5855}, {4102, 897},
	{4588, 7712}, {1073, 6887}, {306, 2185}, {689, 521}, {7877, 297},
	{3073, 2621}, {4733, 0}, {4018, 0}, {2851, 0}, {309, 0}, {3606, 0},
	{5223, 0}, {13, 0}, {6794, 0}, {4919, 0}, {4725, 0},
}

type probabilities [len(symbols) + 1]float64

func logAdd(a, b float64) float64 {
	if math.IsInf(a, -1) {
		return b
	}
	if math.IsInf(b, -1) {
		return a
	}
	return max(a, b) + math.Log1p(math.Exp(-math.Abs(a-b)))
}

// logProbabilities pools upper/lower case and normalizes over the allowed
// alphabet. Working from logits avoids the overflow in exp(logits), and is
// equivalent to restricting and renormalizing the model's softmax output.
func logProbabilities(logits []float32) ([]probabilities, error) {
	if len(logits) == 0 || len(logits)%config.CaptchaModelAlphabet != 0 {
		return nil, fmt.Errorf("unexpected model output length %d", len(logits))
	}
	table := make([]probabilities, len(logits)/config.CaptchaModelAlphabet)
	for step := range table {
		row := logits[step*config.CaptchaModelAlphabet : (step+1)*config.CaptchaModelAlphabet]
		for _, v := range row {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return nil, fmt.Errorf("non-finite model output")
			}
		}
		total := math.Inf(-1)
		for k, indices := range columns {
			v := float64(row[indices[0]])
			if indices[1] != 0 {
				v = logAdd(v, float64(row[indices[1]]))
			}
			table[step][k] = v
			total = logAdd(total, v)
		}
		for k := range table[step] {
			table[step][k] -= total
		}
	}
	return table, nil
}

type beam struct {
	prefix        string
	blank, symbol float64
}

func (b beam) score() float64 { return logAdd(b.blank, b.symbol) }

// proposals is CTC prefix beam search constrained to exactly six symbols.
// A repeated symbol only adds a character when a blank separates its runs.
func proposals(table []probabilities) []string {
	none := math.Inf(-1)
	beams := []beam{{blank: 0, symbol: none}}
	for _, row := range table {
		grown := make(map[string]beam)
		extend := func(prefix string, blank, symbol float64) {
			b, ok := grown[prefix]
			if !ok {
				b = beam{prefix: prefix, blank: none, symbol: none}
			}
			b.blank, b.symbol = logAdd(b.blank, blank), logAdd(b.symbol, symbol)
			grown[prefix] = b
		}
		for _, b := range beams {
			total := b.score()
			extend(b.prefix, total+row[0], none)
			for k, c := range []byte(symbols) {
				p := row[k+1]
				if p <= config.CaptchaBeamFloor {
					continue
				}
				if len(b.prefix) > 0 && b.prefix[len(b.prefix)-1] == c {
					extend(b.prefix, none, b.symbol+p)
					if len(b.prefix) < config.CaptchaLength {
						extend(b.prefix+string(c), none, b.blank+p)
					}
				} else if len(b.prefix) < config.CaptchaLength {
					extend(b.prefix+string(c), none, total+p)
				}
			}
		}
		beams = beams[:0]
		for _, b := range grown {
			beams = append(beams, b)
		}
		sort.Slice(beams, func(i, j int) bool {
			if beams[i].score() == beams[j].score() {
				return beams[i].prefix < beams[j].prefix
			}
			return beams[i].score() > beams[j].score()
		})
		beams = beams[:min(len(beams), config.CaptchaBeamWidth)]
	}
	var readings []string
	for _, b := range beams {
		if len(b.prefix) == config.CaptchaLength {
			readings = append(readings, b.prefix)
		}
	}
	return readings[:min(len(readings), config.CaptchaProposals)]
}

// likelihood sums the probability of every CTC alignment for this reading.
func likelihood(table []probabilities, reading string) float64 {
	labels := make([]int, len(reading)*2+1)
	for i, c := range []byte(reading) {
		for k := range symbols {
			if symbols[k] == c {
				labels[2*i+1] = k + 1
				break
			}
		}
	}
	alpha := make([]float64, len(labels))
	for i := range alpha {
		alpha[i] = math.Inf(-1)
	}
	alpha[0], alpha[1] = table[0][0], table[0][labels[1]]
	for _, row := range table[1:] {
		prev := alpha
		alpha = make([]float64, len(labels))
		for i, k := range labels {
			a := prev[i]
			if i > 0 {
				a = logAdd(a, prev[i-1])
			}
			if i > 1 && k != 0 && k != labels[i-2] {
				a = logAdd(a, prev[i-2])
			}
			alpha[i] = a + row[k]
		}
	}
	return logAdd(alpha[len(alpha)-1], alpha[len(alpha)-2])
}

func rank(tables [][]probabilities) []string {
	found := make(map[string]float64)
	for _, table := range tables {
		for _, reading := range proposals(table) {
			found[reading] = 0
		}
	}
	readings := make([]string, 0, len(found))
	for reading := range found {
		for _, table := range tables {
			found[reading] += likelihood(table, reading)
		}
		readings = append(readings, reading)
	}
	sort.Slice(readings, func(i, j int) bool {
		if found[readings[i]] == found[readings[j]] {
			return readings[i] < readings[j]
		}
		return found[readings[i]] > found[readings[j]]
	})
	return readings[:min(len(readings), config.CaptchaReadings)]
}
