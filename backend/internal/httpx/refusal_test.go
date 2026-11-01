// SPDX-License-Identifier: MIT

package httpx

import (
	"io"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// stalledBody sends a prefix and then nothing until it is closed.
type stalledBody struct {
	prefix []byte
	closed chan struct{}
}

func (b *stalledBody) Read(p []byte) (int, error) {
	if len(b.prefix) > 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		return n, nil
	}
	<-b.closed
	return 0, io.ErrUnexpectedEOF
}

func (b *stalledBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

// TestRefusalSampleHoldsFailoverBriefly: a route's refusal is read for its
// reason for at most config.RouteRefusalBodyWait, a tenth of a second, and
// what arrived by then is kept.
func TestRefusalSampleHoldsFailoverBriefly(t *testing.T) {
	if config.RouteRefusalBodyWait > 100*time.Millisecond {
		t.Fatalf("RouteRefusalBodyWait is %v, more than the 100 ms a refusal may hold failover", config.RouteRefusalBodyWait)
	}
	body := &stalledBody{prefix: []byte("address refused"), closed: make(chan struct{})}
	start := time.Now()
	sample := refusalSample(body, config.RouteRefusalBodyWait)
	if took := time.Since(start); took > config.RouteRefusalBodyWait+150*time.Millisecond {
		t.Errorf("the sample held failover %v", took)
	}
	if string(sample) != "address refused" {
		t.Errorf("sample %q, want what arrived", sample)
	}
}
