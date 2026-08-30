package download

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLowOnSpace(t *testing.T) {
	const floor = 10 << 30

	cases := []struct {
		name        string
		minFree     int64
		free, total int64
		want        bool
	}{
		{"room to spare", floor, 40 << 30, 100 << 30, false},
		{"exactly at the floor is enough", floor, floor, 100 << 30, false},
		{"one byte under is not", floor, floor - 1, 100 << 30, true},
		{"a full disk", floor, 0, 100 << 30, true},
		// A destination that could not be measured reports zero for both.
		// Refusing to download because the check itself failed would be a
		// worse failure than the one it guards against.
		{"unmeasurable destination", floor, 0, 0, false},
		// Zero is the documented way to turn the floor off.
		{"floor disabled", 0, 0, 100 << 30, false},
		{"negative floor is disabled too", -1, 0, 100 << 30, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{minFree: tc.minFree}
			m.diskFree.Store(tc.free)
			m.diskTotal.Store(tc.total)
			if got := m.lowOnSpace(); got != tc.want {
				t.Errorf("lowOnSpace() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The floor gates starting, and holds rather than fails: the queue waits and
// picks up by itself once room is made, so clearing space is the whole of the
// remedy and nothing has to be re-added.
func TestQueueWaitsForSpaceAndResumes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "4")
		_, _ = w.Write([]byte("data"))
	}))
	defer server.Close()

	m, _ := newTestManager(t)
	// A floor the destination cannot satisfy, as if the disk were nearly
	// full. The sampler would overwrite these on its own schedule, so the
	// floor is set above the real free space rather than the figures faked.
	m.minFree = 1 << 62

	jobID, err := m.Add(server.URL+"/held.bin", "")
	if err != nil {
		t.Fatal(err)
	}
	itemID := waitForItem(t, m, jobID)

	// It must sit still rather than fail.
	if waitForCond(2*time.Second, func() bool {
		s := itemStatus(m, jobID, itemID)
		return s == StatusRunning || s == StatusDone || s == StatusFailed
	}) {
		t.Fatalf("item reached %q with no room to write it", itemStatus(m, jobID, itemID))
	}
	if got := itemStatus(m, jobID, itemID); got != StatusQueued {
		t.Errorf("status = %q, want it queued and waiting", got)
	}

	// Room is made: nothing else should be needed.
	m.minFree = 0
	m.signal()

	if !waitForCond(20*time.Second, func() bool {
		return itemStatus(m, jobID, itemID) == StatusDone
	}) {
		t.Fatalf("the queue did not pick up once there was room (status %q)",
			itemStatus(m, jobID, itemID))
	}
}

// The figure has to reach the browser, or a queue sitting still is
// indistinguishable from one that is merely slow.
func TestSnapshotCarriesTheFloor(t *testing.T) {
	m, _ := newTestManager(t)
	m.minFree = 7 << 30
	if got := m.Snapshot().DiskMinFree; got != 7<<30 {
		t.Errorf("DiskMinFree = %d, want %d", got, int64(7<<30))
	}
}
