package download

import "time"

// speedMeter turns a byte counter into a stable throughput reading.
//
// A single interval is far too noisy to make decisions on: a transfer that
// is steadily managing a megabyte a second still reports wild swings from
// one two-second window to the next, depending on where chunk boundaries
// and server pauses happen to fall. Opening connections off that would be
// erratic, so readings are averaged over a window and are only offered once
// the window has actually filled.
type speedMeter struct {
	samples []float64
	next    int
	filled  bool
	last    int64
}

// newSpeedMeter builds a meter averaging over window samples.
func newSpeedMeter(window int, start int64) *speedMeter {
	if window < 1 {
		window = 1
	}
	return &speedMeter{samples: make([]float64, window), last: start}
}

// observe records the throughput since the previous call and reports the
// windowed average. stable is false until enough samples have accumulated
// for the average to mean anything.
func (s *speedMeter) observe(total int64, elapsed time.Duration) (rate float64, stable bool) {
	if elapsed <= 0 {
		return s.average(), s.filled
	}

	delta := total - s.last
	s.last = total
	if delta < 0 {
		delta = 0
	}

	s.samples[s.next] = float64(delta) / elapsed.Seconds()
	s.next++
	if s.next >= len(s.samples) {
		s.next = 0
		s.filled = true
	}
	return s.average(), s.filled
}

// average is the mean of the samples collected so far.
func (s *speedMeter) average() float64 {
	count := len(s.samples)
	if !s.filled {
		count = s.next
	}
	if count == 0 {
		return 0
	}
	var total float64
	for _, v := range s.samples[:count] {
		total += v
	}
	return total / float64(count)
}

// reset clears the history, so the settling period after a change does not
// pollute the next decision.
func (s *speedMeter) reset(total int64) {
	s.next, s.filled, s.last = 0, false, total
	clear(s.samples)
}
