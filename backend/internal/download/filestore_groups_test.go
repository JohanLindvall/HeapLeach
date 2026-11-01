// SPDX-License-Identifier: MIT

package download

import (
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// The two services of one file store count free downloads per address
// apart, so a queue holding both runs one free download of each at once:
// a transfer of one service neither fills the other's host limit nor
// drains its proxy demand.
func TestTheTwoFileStoreServicesArePacedApart(t *testing.T) {
	m := busyManager(t)
	attachPool(t, m, []string{"http://proxy.example.test:80"})
	job := addProxyFiles(m, 4, nil)
	boom := &extractor.Pace{Files: 1, Streams: 1, Group: config.GroupFileBoom, PerRoute: true}
	job.Items[2].pace, job.Items[3].pace = boom, boom
	m.proxyEnabled, m.limit = true, 4
	m.mu.Lock()
	defer m.mu.Unlock()
	if want := m.proxyDemandLocked(); want[config.GroupKeep2Share] != 2 || want[config.GroupFileBoom] != 2 {
		t.Fatalf("demand %v; want two files of each service", want)
	}
	// One service's direct transfer holds back only that service.
	m.running, m.hostActive[groupKey(config.GroupKeep2Share)] = 1, 1
	if !m.hostFullLocked(job.Items[0]) {
		t.Error("a second free download of the same service was let through on the address")
	}
	if m.hostFullLocked(job.Items[2]) {
		t.Error("the other service's file waited for this one's free download")
	}
	if want := m.proxyDemandLocked(); want[config.GroupKeep2Share] != 0 || want[config.GroupFileBoom] != 2 {
		t.Errorf("demand %v; want the other service's unaffected", want)
	}
	m.running = 0
	clear(m.hostActive)
}

// A route that one service holds, or is waiting out, is still the other's
// to lease: each service is a proxy scope of its own.
func TestAFileStoreServicesLeaseLeavesTheOthersRouteFree(t *testing.T) {
	m := busyManager(t)
	pool := attachPool(t, m, []string{"http://proxy.example.test:80"})
	k2s := pool.Acquire(config.GroupKeep2Share, "", 1)
	if k2s == nil {
		t.Fatal("no route for the first service")
	}
	defer k2s.Release()
	if pool.Acquire(config.GroupKeep2Share, "", 1) != nil {
		t.Fatal("the route took a second free download of the same service")
	}
	boom := pool.Acquire(config.GroupFileBoom, "", 1)
	if boom == nil {
		t.Fatal("the other service could not lease a route the first one holds")
	}
	defer boom.Release()
	if pool.Acquire(config.GroupFileBoom, "", 1) != nil {
		t.Error("the other service is not held to one free download per route of its own")
	}
}
