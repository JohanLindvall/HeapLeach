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
		got := m.proxyDemandLocked()["keep2share"]
		m.mu.Unlock()
		if got != want {
			t.Fatalf("proxy demand=%d, want %d", got, want)
		}
	}
	check(4)
	// Two workers on other hosts leave two places for K2S.
	m.running = 2
	check(2)
	m.proxyRunning["keep2share"] = 1
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
	m.running = 0
	clear(m.proxyRunning)
}

func TestProxyDiscoverySharesConcurrencyAcrossServices(t *testing.T) {
	m := busyManager(t)
	attachPool(t, m, []string{"http://proxy.example.test:80"})
	job := addProxyFiles(m, 4, nil)
	job.Items[1].pace = &extractor.Pace{Group: "fileboom", PerRoute: true, Files: 1}
	job.Items[3].pace = job.Items[1].pace
	m.proxyEnabled, m.limit = true, 3
	m.mu.Lock()
	defer m.mu.Unlock()
	want := m.proxyDemandLocked()
	if want["keep2share"] != 2 || want["fileboom"] != 1 {
		t.Fatalf("one concurrency budget must serve both queues: %v", want)
	}
	// A direct K2S worker drains only K2S; FileBoom can still use proxies.
	m.running, m.hostActive["group:keep2share"] = 1, 1
	want = m.proxyDemandLocked()
	if want["keep2share"] != 0 || want["fileboom"] != 2 {
		t.Fatalf("direct K2S blocked FileBoom demand: %v", want)
	}
	// Another service's lease cannot conceal that unleased K2S transfer.
	m.running, m.proxyRunning["fileboom"] = 2, 1
	if !m.hostFullLocked(job.Items[0]) {
		t.Fatal("FileBoom's active proxy concealed an unleased K2S transfer")
	}
	m.running = 0
	clear(m.proxyRunning)
}
