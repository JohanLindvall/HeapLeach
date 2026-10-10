// SPDX-License-Identifier: MIT

package proxy

import (
	"cmp"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Query bounds the independent inventory endpoint. Large feeds never ride
// along with queue snapshots, and sorting/filtering happens on the server.
type Query struct {
	Offset, Limit        int
	Search, Status, Sort string
}

type Row struct {
	ID            string    `json:"id"`
	URL           string    `json:"url"`
	Source        string    `json:"source"`
	Status        string    `json:"status"`
	Score         float64   `json:"score"`
	Throughput    float64   `json:"throughput"`
	CurrentSpeed  float64   `json:"currentSpeed"`
	SuccessRate   float64   `json:"successRate"`
	Requests      int       `json:"requests"`
	Active        int       `json:"active"`
	CooldownUntil time.Time `json:"cooldownUntil,omitzero"`
	LastSuccess   time.Time `json:"lastSuccess,omitzero"`
}

type Summary struct {
	Total     int `json:"total"`
	Available int `json:"available"`
	Active    int `json:"active"`
	Cooling   int `json:"cooling"`
	Untested  int `json:"untested"`
}

type SourceView struct {
	URL        string    `json:"url"`
	Count      int       `json:"count"`
	Fetched    time.Time `json:"fetched,omitzero"`
	Refreshing bool      `json:"refreshing"`
	Failed     bool      `json:"failed"`
}

type Page struct {
	Rows    []Row        `json:"rows"`
	Total   int          `json:"total"`
	Offset  int          `json:"offset"`
	Limit   int          `json:"limit"`
	Summary Summary      `json:"summary"`
	Sources []SourceView `json:"sources"`
}

func EmptyPage(q Query) Page {
	limit := q.Limit
	if limit <= 0 {
		limit = config.ProxyPageSize
	}
	return Page{Rows: []Row{}, Sources: []SourceView{}, Limit: min(limit, config.ProxyMaxPageSize)}
}

// Page reports recent measurements and evaluates the selector's score at the
// mean success probability. Reading the UI never draws from the selector's
// random source or changes its priors. Endpoint credentials are redacted.
func (p *Pool) Page(site string, q Query) Page {
	page := EmptyPage(q)
	search := strings.ToLower(strings.TrimSpace(q.Search))
	now := time.Now()
	p.mu.Lock()
	prior := p.scorePrior(site)
	active := make(map[string]int)
	cooling := make(map[string]time.Time)
	for _, e := range p.entries {
		s := e.readStat(site)
		key := identity(e.URL)
		active[key] += s.active
		if s.Until.After(cooling[key]) {
			cooling[key] = s.Until
		}
	}
	for _, e := range p.entries {
		s := e.readStat(site)
		enabled := p.enabled(e)
		if !enabled && s.active == 0 {
			continue
		}
		row := Row{ID: routeID(e.URL), URL: redacted(e.URL), Source: "discovered",
			Status: "available", Throughput: s.BytesPerSecond, Active: s.active,
			Requests: s.Tries, LastSuccess: e.LastSuccess}
		if e.URL == Direct {
			row.Source = "direct"
		} else if slices.Contains(p.static, e.URL) {
			row.Source = "manual"
		}
		row.Score = prior.meanRate(s)
		if s.OK+s.Bad > 0 {
			row.SuccessRate = s.OK / (s.OK + s.Bad)
		}
		until := cooling[identity(e.URL)]
		if e.Until.After(until) {
			until = e.Until
		}
		if until.After(now) {
			row.CooldownUntil = until
		}
		page.Summary.Total++
		page.Summary.Active += s.active
		if s.Tries == 0 {
			page.Summary.Untested++
		}
		switch {
		case !enabled:
			row.Status = "finishing"
		case s.active > 0:
			row.Status = "active"
		case until.After(now):
			row.Status = "cooling"
			page.Summary.Cooling++
		case active[identity(e.URL)] > 0:
			row.Status = "busy"
		default:
			page.Summary.Available++
			if s.Tries == 0 {
				row.Status = "untested"
			}
		}
		if search != "" && !strings.Contains(strings.ToLower(row.URL), search) {
			continue
		}
		if q.Status != "" && q.Status != "all" && row.Status != q.Status &&
			!(q.Status == "available" && row.Status == "untested") {
			continue
		}
		page.Rows = append(page.Rows, row)
	}
	for _, raw := range p.feeds {
		view := SourceView{URL: redacted(raw)}
		if s := p.sources[raw]; s != nil {
			view.Count, view.Fetched, view.Refreshing, view.Failed = len(s.Members), s.Fetched, s.refreshing, s.Failed
		}
		page.Sources = append(page.Sources, view)
	}
	p.mu.Unlock()

	// Sorting and encoding copied values never hold up a route acquisition.
	slices.SortFunc(page.Rows, func(a, b Row) int {
		var order int
		switch q.Sort {
		case "address":
			order = strings.Compare(a.URL, b.URL)
		case "throughput":
			order = cmp.Compare(b.Throughput, a.Throughput)
		case "success":
			order = cmp.Compare(b.SuccessRate, a.SuccessRate)
		default:
			order = cmp.Compare(b.Score, a.Score)
		}
		if order == 0 {
			order = strings.Compare(a.ID, b.ID)
		}
		return order
	})
	page.Total = len(page.Rows)
	page.Offset = min(max(q.Offset, 0), max(page.Total-1, 0)/page.Limit*page.Limit)
	page.Rows = page.Rows[page.Offset:min(page.Offset+page.Limit, page.Total)]
	return page
}

func redacted(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid endpoint"
	}
	return u.Redacted()
}
