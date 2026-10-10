// SPDX-License-Identifier: MIT

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestProxyPageUsesMeasuredScoringAndRedactsCredentials(t *testing.T) {
	fast, slow := "http://reader:private-password@fast.example.test:80", "http://slow.example.test:80"
	p := testPool(t, []string{fast, slow}, nil)
	for _, raw := range []string{fast, slow} {
		s := p.entries[raw].stat("keep2share")
		s.Tries, s.OK, s.Bad, s.Duration, s.BytesPerSecond = 10, 8, 2, 2, 1<<20
	}
	p.entries[fast].stat("keep2share").BytesPerSecond = 8 << 20
	a, b := p.Page("keep2share", Query{}), p.Page("keep2share", Query{})
	if len(a.Rows) != 2 || a.Rows[0].ID != routeID(fast) || a.Rows[0].Score <= a.Rows[1].Score || a.Rows[0].Score != b.Rows[0].Score {
		t.Fatalf("unstable or incorrect throughput score: %+v", a.Rows)
	}
	if a.Rows[0].SuccessRate != 0.8 || a.Rows[0].Throughput != 8<<20 || a.Rows[0].Requests != 10 {
		t.Fatalf("incorrect measurements: %+v", a.Rows[0])
	}
	body, _ := json.Marshal(a)
	if strings.Contains(string(body), "private-password") {
		t.Fatal("inventory exposed proxy credentials")
	}
	if len(p.priors) != 0 || p.last["keep2share"] != "" {
		t.Fatal("reading the inventory changed the selector")
	}
	unknown := p.Page("another-service", Query{})
	if unknown.Summary.Untested != 2 || unknown.Rows[0].Throughput != 0 {
		t.Fatal("K2S measurements leaked into another service")
	}
}

func TestProxyPageBoundsFiltersAndSortsBeforePagination(t *testing.T) {
	var endpoints []string
	for i := range config.ProxyMaxPageSize + 5 {
		endpoints = append(endpoints, fmt.Sprintf("http://route-%03d.example.test:80", i))
	}
	p := testPool(t, endpoints, nil)
	q := Query{Sort: "address", Offset: 5, Limit: 5}
	page := p.Page("keep2share", q)
	var got []string
	for _, row := range page.Rows {
		got = append(got, row.URL)
	}
	if !slices.Equal(got, endpoints[5:10]) || page.Total != len(endpoints) || page.Summary.Untested != len(endpoints) {
		t.Fatalf("incorrect page: %+v", page)
	}
	if page := p.Page("keep2share", Query{Limit: config.ProxyMaxPageSize + 100}); len(page.Rows) != config.ProxyMaxPageSize {
		t.Fatalf("unbounded page: %d", len(page.Rows))
	}
	page = p.Page("keep2share", Query{Search: "ROUTE-00", Sort: "address", Limit: 3})
	if page.Total != 10 || len(page.Rows) != 3 {
		t.Fatalf("search applied after pagination: %+v", page)
	}
	page = p.Page("keep2share", Query{Status: "active", Offset: 200})
	if page.Total != 0 || page.Offset != 0 || page.Rows == nil {
		t.Fatalf("empty page is not usable by the UI: %+v", page)
	}
}

func TestProxyPageShowsAddressCooldownAndRemovedActiveRoute(t *testing.T) {
	a, alias, b := "http://same.example.test:80", "socks5h://same.example.test:1080", "http://other.example.test:80"
	p := testPool(t, []string{a, alias, b}, nil)
	lease := p.Acquire("keep2share", routeID(a), 1)
	lease.observe(httpx.RouteObservation{Status: 200, Bytes: 1 << 20, Duration: time.Second})
	until := time.Now().Add(time.Hour)
	lease.Cooldown(until)
	page := p.Page("keep2share", Query{Status: "cooling"})
	if page.Total != 1 || !page.Rows[0].CooldownUntil.Equal(until) {
		t.Fatalf("same-address cooldown is missing: %+v", page)
	}
	c, err := ParseConfiguration([]string{b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Configure(c, false)
	page = p.Page("keep2share", Query{Status: "finishing"})
	if page.Total != 1 || page.Rows[0].ID != lease.ID() || page.Summary.Active != 1 {
		t.Fatalf("lost removed route's active transfer: %+v", page)
	}
	other := p.Acquire("keep2share", lease.ID(), 1)
	if other == nil || other.ID() != routeID(b) {
		t.Fatal("removed endpoint was leased again")
	}
	other.Release()
	lease.Release()
	if page := p.Page("keep2share", Query{}); page.Total != 1 {
		t.Fatal("idle removed endpoint remained in the current inventory")
	}
}

func TestRemovedFeedCannotRepopulateTheLivePool(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-finish
		fmt.Fprint(w, "http://discarded.example.test:80")
	}))
	defer srv.Close()
	p := testPool(t, []string{Direct}, []string{srv.URL})
	done := make(chan struct{})
	go func() { p.Refresh(context.Background()); close(done) }()
	<-started
	c, _ := ParseConfiguration([]string{Direct}, nil)
	p.Configure(c, true)
	close(finish)
	<-done
	if len(p.entries) != 1 || len(p.Page("keep2share", Query{}).Sources) != 0 {
		t.Fatal("an obsolete discovery result changed the pool")
	}
}
