// SPDX-License-Identifier: MIT

package proxy

import (
	"errors"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A route shelved as dead or broken gives up its client when its last lease
// ends: it will not be leased again for a while, and route hunting tries
// thousands like it. One still in use, or healthy, keeps it.
func TestAShelvedRouteGivesUpItsClient(t *testing.T) {
	p := testPool(t, []string{"http://dead.example.test:80", "http://fine.example.test:80"}, nil)
	dead, fine := p.entries[p.static[0]], p.entries[p.static[1]]

	first := p.Acquire("keep2share", routeID(dead.URL), 1)
	second := p.Acquire("fileboom", routeID(dead.URL), 1)
	if first == nil || second == nil || first.entry != dead || second.entry != dead || dead.client == nil {
		t.Fatal("did not lease the route twice")
	}
	first.observe(httpx.RouteObservation{Err: errors.New("dial tcp: connection refused"), Duration: time.Second})
	if !dead.Until.After(time.Now()) {
		t.Fatal("test set-up: the failure did not shelve the route")
	}
	first.Release()
	if dead.client == nil {
		t.Fatal("the client was dropped while another lease was still using it")
	}
	if second.Context(t.Context()) == nil || second.client == nil {
		t.Fatal("the remaining lease lost its client")
	}
	second.Release()
	if dead.client != nil {
		t.Fatal("a shelved route kept its client after its last lease")
	}

	healthy := p.Acquire("keep2share", routeID(fine.URL), 1)
	if healthy == nil || healthy.entry != fine {
		t.Fatal("did not lease the healthy route")
	}
	healthy.Release()
	if fine.client == nil {
		t.Fatal("a healthy route gave up its client")
	}

	p.mu.Lock()
	dead.Until = time.Time{} // its cooldown over
	p.mu.Unlock()
	again := p.Acquire("keep2share", routeID(dead.URL), 1)
	if again == nil || again.entry != dead || again.client == nil {
		t.Fatal("a route leased again after its cooldown has no client")
	}
	again.Release()
}
