// SPDX-License-Identifier: MIT

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

func normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == Direct {
		return Direct, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("malformed proxy URL")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || u.Hostname() == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" ||
		strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return "", errors.New("proxy must be a host and port, without a path, query or fragment")
	}
	switch u.Scheme {
	case "http", "https", "socks5h":
	case "socks5":
		u.Scheme = "socks5h" // remote DNS, as in amzscrape
	default:
		return "", fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	u.Host, u.Path = strings.ToLower(u.Host), ""
	return u.String(), nil
}

// parseFeed accepts plain endpoints, JSON strings, and Proxifly records. A
// malformed/nonempty feed retains the previous inventory; [] is a valid empty
// list. Feed-provided rankings are deliberately ignored.
func parseFeed(body []byte) ([]string, error) {
	body = bytes.TrimSpace(bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf}))
	var rows []json.RawMessage
	if len(body) > 0 && (body[0] == '[' || body[0] == '{') {
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, err
		}
	} else {
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			data, _ := json.Marshal(line)
			rows = append(rows, data)
		}
	}
	seen := make(map[string]bool)
	var result []string
	for _, row := range rows {
		var raw string
		if json.Unmarshal(row, &raw) != nil {
			var record struct {
				Proxy string `json:"proxy"`
			}
			if json.Unmarshal(row, &record) != nil {
				continue
			}
			raw = record.Proxy
		}
		u, err := normalize(raw)
		// A feed must never add the user's own address to their policy.
		if err != nil || u == Direct || seen[u] {
			continue
		}
		seen[u] = true
		result = append(result, u)
		if len(result) >= config.ProxyMaxEntries {
			break
		}
	}
	if len(rows) > 0 && len(result) == 0 {
		return nil, errors.New("proxy feed contains no usable endpoints")
	}
	return result, nil
}

func (p *Pool) fetch(ctx context.Context, src string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, config.ProxyFeedTimeout)
	defer cancel()
	req, err := p.client.NewRequest(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.DoOnce(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy feed: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, config.ProxyFeedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > config.ProxyFeedBytes {
		return nil, errors.New("proxy feed is too large")
	}
	return parseFeed(body)
}

// Refresh retains surviving health and each failed source's last good list.
// Routine attempts are a day apart, including across restarts or failures.
// Earlier top-ups require runnable demand and too few usable, scored routes,
// and are at least five minutes apart. Network I/O holds neither the pool
// lock nor a Bolt transaction.
func (p *Pool) Refresh(ctx context.Context) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if p.configDirty {
		if err := p.saveAll(); err != nil {
			p.log.Error("could not save proxy inventory", "err", err)
		} else {
			p.configDirty = false
			clear(p.dirty)
		}
	} else if err := p.flushMeasurements(); err != nil {
		p.log.Error("could not save proxy measurements", "err", err)
	}
	feeds := slices.Clone(p.feeds)
	p.mu.Unlock()
	for _, src := range feeds {
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		if !p.discovery || !slices.Contains(p.feeds, src) {
			p.mu.Unlock()
			continue
		}
		now := time.Now()
		s := p.sources[src]
		if s == nil {
			s = &source{}
			p.sources[src] = s
		}
		elapsed := now.Sub(s.Attempted)
		due := s.Attempted.IsZero() || elapsed >= config.ProxyRefresh ||
			(elapsed >= config.ProxyRefreshRetry && p.shortOfRoutes(now))
		if !due || s.refreshing {
			p.mu.Unlock()
			continue
		}
		s.Attempted, s.refreshing = now, true
		p.mu.Unlock()
		members, err := p.fetch(ctx, src)
		p.mu.Lock()
		s.refreshing = false
		if p.closed {
			p.mu.Unlock()
			return
		}
		if !p.discovery || !slices.Contains(p.feeds, src) {
			p.mu.Unlock()
			continue
		}
		s.Failed = err != nil && ctx.Err() == nil
		if err == nil {
			s.Fetched, s.Members = time.Now(), members
			for _, raw := range members {
				e := p.ensure(raw, s.Fetched)
				e.LastSeen = s.Fetched
				if !slices.Contains(e.Origins, src) {
					e.Origins = append(e.Origins, src)
				}
			}
		} else if ctx.Err() == nil {
			u, _ := url.Parse(src)
			p.log.Warn("proxy feed unavailable; keeping saved inventory", "feed", u.Redacted(), "err", err)
		}
		p.retire(time.Now())
		if err := p.saveAll(); err != nil {
			p.log.Error("could not save proxy inventory", "err", err)
		}
		p.mu.Unlock()
	}
}

// shortOfRoutes judges capacity, not the size of a feed. Untried entries and
// weak scores cannot conceal a depleted pool. A standby route must have
// succeeded and score at least as well as the learned prior for an untried
// route; this uses the UI's mean score, without drawing random samples.
// Active leases count during their first measurement window; after that,
// their measured score must qualify too. Ports and protocols on one address
// count only once, just as in Acquire.
// Caller holds mu.
func (p *Pool) shortOfRoutes(now time.Time) bool {
	for site, wanted := range p.demand {
		if wanted <= 0 {
			continue
		}
		prior := p.scorePrior(site)
		threshold := prior.meanRate(siteStat{})
		cooling, ready := make(map[string]bool), make(map[string]bool)
		for _, e := range p.entries {
			if e.readStat(site).Until.After(now) {
				cooling[identity(e.URL)] = true
			}
		}
		for _, e := range p.entries {
			key, s := identity(e.URL), e.readStat(site)
			if e.Until.After(now) || cooling[key] {
				continue
			}
			starting := s.active > 0 && s.BytesPerSecond == 0
			scored := s.OK > 0 && prior.meanRate(s) >= threshold
			if starting || ((p.enabled(e) || s.active > 0) && scored) {
				ready[key] = true
				if len(ready) >= wanted {
					break
				}
			}
		}
		if len(ready) < wanted {
			return true
		}
	}
	return false
}

func (p *Pool) retire(now time.Time) {
	listed := make(map[string]bool)
	for _, src := range p.feeds {
		if s := p.sources[src]; s != nil {
			for _, raw := range s.Members {
				listed[raw] = true
			}
		}
	}
	for raw, e := range p.entries {
		if raw == Direct || listed[raw] || slices.Contains(p.static, raw) {
			continue
		}
		last := e.LastSuccess
		if last.IsZero() {
			last = e.FirstSeen
		}
		active := false
		for _, stat := range e.Sites {
			if stat.active > 0 {
				active = true
				break
			}
		}
		if !active && now.Sub(last) >= config.ProxyRetireAfter {
			if e.client != nil {
				e.client.CloseIdleConnections()
			}
			delete(p.entries, raw)
		}
	}
}
