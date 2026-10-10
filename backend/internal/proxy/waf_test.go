// SPDX-License-Identifier: MIT

package proxy

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestWAFRefusalLeavesOrdinaryHealthSpeedAndAdmissionIntact(t *testing.T) {
	p := testPool(t, []string{"http://one.example.test:80", "http://two.example.test:80"}, nil)
	l := p.Acquire("keep2share", "", 1)
	raw, id := l.entry.URL, l.ID()
	l.observe(httpx.RouteObservation{Status: 200, Bytes: 4 << 20, Duration: time.Second})
	l.Release()
	before, _ := json.Marshal(p.Page("keep2share", Query{}))
	l = p.Acquire("keep2share", id, 1)
	l.observe(httpx.RouteObservation{Status: http.StatusForbidden, Duration: time.Second,
		Err: &httpx.ChallengeError{Err: &httpx.StatusError{Code: 200, URL: "https://example.test/"}}})
	l.Release()
	after, _ := json.Marshal(p.Page("keep2share", Query{}))
	if string(before) != string(after) || p.entries[raw].Fails != 0 || !p.entries[raw].Until.IsZero() {
		t.Fatal("WAF refusal changed ordinary scores, speed, health or cooldown")
	}
	ordinary := p.Acquire("keep2share", id, 1)
	if ordinary == nil || ordinary.ID() != id {
		t.Fatal("WAF refusal prevented an ordinary download")
	}
	ordinary.Release()
	waf := p.AcquireFor("keep2share", id, 1, 1<<20, true)
	if waf == nil || waf.ID() == id {
		t.Fatal("WAF recovery reused the challenged address")
	}
	// Applying the WAF profile must retain the service's per-address cap.
	other := waf.entry
	if duplicate := p.Acquire("keep2share", waf.ID(), 1); duplicate != nil {
		if duplicate.entry == other {
			t.Error("WAF recovery lost service admission")
		}
		duplicate.Release()
	}
	waf.Release()
	path := p.db.Path()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(path, p.static, nil, p.client, p.log)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	e := restored.entries[raw]
	if e.WAF.Fails != 1 || !e.WAF.Until.After(time.Now()) || e.stat("keep2share").BytesPerSecond != 4<<20 {
		t.Fatal("restart lost isolated WAF history or ordinary speed")
	}
}

func TestWAFSelectionReusesSpeedWithoutTrainingOnThrottledTransfers(t *testing.T) {
	p := testPool(t, []string{"http://fast.example.test:80", "http://slow.example.test:80"}, nil)
	for i, raw := range p.static {
		l := p.Acquire("ordinary", routeID(raw), 1)
		l.observe(httpx.RouteObservation{Status: 200, Bytes: int64(8<<20) >> (i * 6), Duration: time.Second})
		l.Release()
		// A capped transfer is responsiveness evidence, not proxy capacity.
		limited := p.Acquire("limited", routeID(raw), 1)
		limited.Constrain()
		limited.Progress(time.Now().Add(time.Minute), 64<<20, time.Second, true)
		limited.observe(httpx.RouteObservation{Status: 200, Bytes: 64 << 20, Duration: time.Second})
		limited.Release()
		// Known free-host limits also must not replace unrestricted speed.
		free := p.Acquire("keep2share", routeID(raw), 1)
		free.observe(httpx.RouteObservation{Status: 200, Bytes: 128 << 20, Duration: time.Second})
		free.Release()
		if p.entries[raw].scoreStat("cloudflare").BytesPerSecond != float64(int64(8<<20)>>(i*6)) {
			t.Fatal("WAF score discarded measured speed or used a throttled sample")
		}
	}
	fast, slow := p.entries[p.static[0]], p.entries[p.static[1]]
	prior := p.scorePrior("cloudflare")
	if prior.entryRate(fast, "cloudflare", true, 0) <= prior.entryRate(slow, "cloudflare", true, 0) {
		t.Fatal("WAF scoring ignored the faster proxy's existing measurements")
	}
	ordinaryBefore := p.scorePrior("ordinary").entryRate(fast, "ordinary", false, 0)
	fast.WAF.Bad = 1000
	if prior.entryRate(fast, "cloudflare", true, 0) >= prior.entryRate(slow, "cloudflare", true, 0) {
		t.Fatal("repeated WAF rejections did not reduce WAF selection score")
	}
	if p.scorePrior("ordinary").entryRate(fast, "ordinary", false, 0) != ordinaryBefore {
		t.Fatal("WAF penalty leaked into ordinary selection")
	}
}
