// SPDX-License-Identifier: MIT

package download

import (
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

func TestProxyDiscoveryDemandTracksRunnableCapacity(t *testing.T) {
	m := busyManager(t)
	job := addProxyFiles(m, 4, nil)
	m.proxyEnabled, m.limit = true, 4
	check := func(want int) {
		t.Helper()
		m.mu.Lock()
		got := m.proxyDemandLocked()
		m.mu.Unlock()
		if got != want {
			t.Fatalf("proxy demand=%d, want %d", got, want)
		}
	}
	check(4)
	// Two workers on other hosts leave two places for K2S.
	m.running = 2
	check(2)
	m.proxyRunning = 1
	m.hostActive["group:keep2share"] = 1
	check(3)
	// A resumed direct transfer must drain before proxies can help.
	m.hostActive["group:keep2share"] = 2
	check(0)
	m.hostActive["group:keep2share"] = 1
	job.Items[0].Status = StatusCanceled
	job.Items[1].notBefore = time.Now().Add(time.Hour)
	job.Items[2].pace = &extractor.Pace{Group: "other", PerRoute: true}
	check(2) // one active K2S transfer and one runnable K2S file
	m.limit = 1
	check(0) // the other host already fills the reduced limit
	m.limit = 4
	m.throttle.setPaused(true)
	check(0)
	m.throttle.setPaused(false)
	m.proxyEnabled = false
	check(0)
	// No workers were actually started by this state-machine test.
	m.running, m.proxyRunning = 0, 0
}
