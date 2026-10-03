package download

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

func TestResolutionConcurrencyAndCancelWhileWaiting(t *testing.T) {
	var requests atomic.Int32
	var canceledRequested atomic.Bool
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.Contains(r.URL.Path, "canceled") {
			canceledRequested.Store(true)
		}
		select {
		case <-release:
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, "<html>Empty listing</html>")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	m, _ := newTestManager(t)
	for i := range config.ResolveConcurrency {
		if _, err := m.Add(fmt.Sprintf("links:%s/%d", srv.URL, i), ""); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 5*time.Second, func() bool { return requests.Load() == int32(config.ResolveConcurrency) })
	id, err := m.Add("links:"+srv.URL+"/canceled", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CancelJob(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.jobs[id].resolving
	})
	if got := requests.Load(); got != int32(config.ResolveConcurrency) {
		t.Fatalf("waiting source reached the network: requests = %d", got)
	}
	close(release)
	m.Close()
	if canceledRequested.Load() {
		t.Fatal("canceled source reached the network after a slot freed")
	}
}
