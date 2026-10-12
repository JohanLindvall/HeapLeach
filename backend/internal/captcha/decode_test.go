// SPDX-License-Identifier: MIT

package captcha

import (
	"math"
	"slices"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

type step map[byte]float64

func spelled(s string) []step {
	var steps []step
	for _, c := range []byte(s) {
		steps = append(steps, step{c: .9}, step{0: .9})
	}
	return steps
}

func scores(t *testing.T, steps []step) []probabilities {
	t.Helper()
	logits := make([]float32, len(steps)*config.CaptchaModelAlphabet)
	for i := range logits {
		logits[i] = -20
	}
	for i, s := range steps {
		for c, p := range s {
			col := 0
			if c != 0 {
				upper := c >= 'A' && c <= 'Z'
				if upper {
					c += 'a' - 'A'
				}
				for k := range symbols {
					if symbols[k] == c {
						col = columns[k+1][0]
						if upper {
							col = columns[k+1][1]
						}
					}
				}
			}
			logits[i*config.CaptchaModelAlphabet+col] = float32(math.Log(p))
		}
		// A symbol outside the challenge alphabet must never outvote a letter.
		logits[i*config.CaptchaModelAlphabet+1] = 10
	}
	table, err := logProbabilities(logits)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func TestDecodeChallengeRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []step
		want  []string
	}{
		{"six including faint character", append(spelled("ab3de"), step{0: .7, '7': .25}, step{0: .9}), []string{"ab3de7"}},
		{"pool letter cases", append(spelled("ab3de"), step{'K': .3, 'k': .3, 'x': .35}, step{0: .9}), []string{"ab3dek", "ab3dex"}},
		{"repeated character separated by blank", spelled("aab3de"), []string{"aab3de"}},
		{"unseparated repeat is one character", append([]step{{'a': .9}}, spelled("ab3de7")...), []string{"ab3de7"}},
		{"ambiguous bar retains alternative", append(spelled("ab3de"), step{'I': .5, 'l': .45}, step{0: .9}), []string{"ab3dei", "ab3del"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rank([][]probabilities{scores(t, tc.steps)})
			if len(got) < len(tc.want) || !slices.Equal(got[:len(tc.want)], tc.want) {
				t.Fatalf("ranked %q; want prefix %q", got, tc.want)
			}
		})
	}
}

func TestBothViewsContributeToRanking(t *testing.T) {
	a := scores(t, append(spelled("ab3de"), step{'x': .55, 'k': .45}, step{0: .9}))
	b := scores(t, append(spelled("ab3de"), step{'x': .1, 'k': .9}, step{0: .9}))
	if got := rank([][]probabilities{a, b}); len(got) < 2 || got[0] != "ab3dek" || got[1] != "ab3dex" {
		t.Fatalf("joint ranking: %q", got)
	}
}

func TestInvalidModelOutput(t *testing.T) {
	for _, value := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		logits := make([]float32, config.CaptchaModelAlphabet)
		logits[1] = value
		if _, err := logProbabilities(logits); err == nil {
			t.Fatalf("accepted non-finite model output %v", value)
		}
	}
	for _, size := range []int{0, config.CaptchaModelAlphabet - 1, config.CaptchaModelAlphabet + 1} {
		if _, err := logProbabilities(make([]float32, size)); err == nil {
			t.Fatalf("accepted output of length %d", size)
		}
	}
	// Large finite logits are safe: a softmax implemented with exp(1000)
	// would overflow, while the decoder operates directly in log space.
	logits := make([]float32, config.CaptchaModelAlphabet)
	for i := range logits {
		logits[i] = 1000
	}
	table, err := logProbabilities(logits)
	if err != nil || math.IsNaN(table[0][0]) {
		t.Fatalf("large logits: %v, %v", table, err)
	}
}
