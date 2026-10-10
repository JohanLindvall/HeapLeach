// SPDX-License-Identifier: MIT

package proxy

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Services are the download protocols currently opted into per-address routing.
func Services() []string { return []string{"keep2share", "fileboom", "cloudflare"} }

// Configuration is immutable once published. ParseConfiguration owns its slices.
type Configuration struct {
	Endpoints []string
	Feeds     []string
}

func ParseConfiguration(endpoints, feeds []string) (Configuration, error) {
	c := Configuration{Endpoints: []string{}, Feeds: []string{}}
	if len(endpoints) > config.ProxyMaxEntries || len(feeds) > config.ProxyMaxFeeds {
		return c, errors.New("too many proxy endpoints or discovery feeds")
	}
	seen := make(map[string]bool)
	for _, raw := range endpoints {
		u, err := normalize(raw)
		if err != nil {
			return c, fmt.Errorf("proxy endpoint: %w", err)
		}
		if !seen[u] {
			c.Endpoints = append(c.Endpoints, u)
			seen[u] = true
		}
	}
	for _, raw := range feeds {
		raw = strings.TrimSpace(raw)
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return c, errors.New("proxy feed must be an HTTP(S) URL")
		}
		if !slices.Contains(c.Feeds, raw) {
			c.Feeds = append(c.Feeds, raw)
		}
	}
	return c, nil
}

// Configure publishes a validated configuration without disk or network I/O.
// Existing leases keep their client even when their endpoint is removed.
// The background refresh persists newly added inventory and fetches new feeds.
func (p *Pool) Configure(c Configuration, discovery bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.static, p.feeds = slices.Clone(c.Endpoints), slices.Clone(c.Feeds)
	p.discovery = discovery
	for _, raw := range p.static {
		if p.entries[raw] == nil {
			p.configDirty = true
		}
		p.ensure(raw, time.Now())
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// SetDemand publishes every service's runnable route demand atomically, so
// discovery cannot act on a mix of old and new concurrency allocations.
// An empty map suppresses shortage-driven refreshes while idle or paused.
// Demand is not persisted: restoring a pool does not mean a queue is ready.
func (p *Pool) SetDemand(routes map[string]int) {
	wanted := make(map[string]int, len(routes))
	for site, n := range routes {
		if n > 0 {
			wanted[site] = n
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || maps.Equal(p.demand, wanted) {
		return
	}
	p.demand = wanted
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
