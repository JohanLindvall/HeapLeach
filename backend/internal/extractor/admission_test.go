// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// nestedSite is a synthetic nested discovery workload, served locally and
// counted at the HTTP boundary: a root page whose sources are listings,
// each listing resolving its leaves with a request apiece. The roots
// expand through the real registry (expandSources → follow → probe →
// Extract), so whatever an extraction carries reaches the nested ones.
type nestedSite struct {
	srv *httptest.Server
	// fanout is how many listings a root names and leaves a listing names.
	fanout int
	// hold keeps each leaf's answer open until want leaves are active or
	// the hold times out, so the peak is what the bounds allow rather
	// than what the scheduler managed.
	want int32
	hold time.Duration
	// slowBody, when set, sends the leaf's headers at once and its body
	// only after the hold: a request whose answer is still being read.
	slowBody bool

	active, peak, total atomic.Int32
	reached             chan struct{}
	once                sync.Once
}

func newNestedSite(t *testing.T, fanout int, want int32, hold time.Duration) *nestedSite {
	t.Helper()
	s := &nestedSite{fanout: fanout, want: want, hold: hold, reached: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *nestedSite) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(httpx.HeaderContentType, "text/html")
	s.total.Add(1)
	if !strings.HasPrefix(r.URL.Path, "/leaf/") {
		_, _ = io.WriteString(w, "<html><body>"+r.URL.Path+"</body></html>")
		return
	}
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for p := s.peak.Load(); n > p && !s.peak.CompareAndSwap(p, n); p = s.peak.Load() {
	}
	if n >= s.want {
		s.once.Do(func() { close(s.reached) })
	}
	if s.slowBody {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
	}
	select {
	case <-s.reached:
	case <-time.After(s.hold):
	case <-r.Context().Done():
	}
	_, _ = io.WriteString(w, "<html><body>leaf</body></html>")
}

// nestedExtractor is a root or a listing of the nested site, claimed by its
// path's first segment.
type nestedExtractor struct {
	kind     string // "root" or "list"
	site     *nestedSite
	client   *httpx.Client
	registry *Registry
}

func (e *nestedExtractor) Name() string { return "nested-" + e.kind }
func (e *nestedExtractor) Shape(u *url.URL) bool {
	return strings.HasPrefix(u.Path, "/"+e.kind+"/")
}
func (e *nestedExtractor) Recognise(_ context.Context, p *Page) bool { return e.Shape(p.Final) }
func (e *nestedExtractor) Recognises() string                        { return "the nested test site's " + e.kind + "s" }

func (e *nestedExtractor) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	id := strings.TrimPrefix(u.Path, "/"+e.kind+"/")
	if e.kind == "root" {
		var lists []string
		for i := range e.site.fanout {
			lists = append(lists, fmt.Sprintf("%s/list/%s-%d", e.site.srv.URL, id, i))
		}
		x := expandSources(ctx, e.registry, lists, opts)
		if err := x.unresolved(ctx, e.Name(), "listings", u.String()); err != nil {
			return nil, err
		}
		return &Result{Title: id, Files: x.files}, nil
	}
	leaves := make([]string, e.site.fanout)
	for j := range leaves {
		leaves[j] = fmt.Sprintf("%s/leaf/%s-%d", e.site.srv.URL, id, j)
	}
	files, _, err := readListing(ctx, e.Name(), "leaves", u.String(), leaves, func(ctx context.Context, leaf string) ([]File, error) {
		if _, err := e.client.GetString(ctx, leaf, nil); err != nil {
			return nil, err
		}
		return []File{{Name: leaf[strings.LastIndex(leaf, "/")+1:] + ".bin", URL: leaf, Size: -1}}, nil
	})
	if err != nil {
		return nil, err
	}
	return &Result{Title: id, Files: files}, nil
}

// nestedRegistry is a registry of the nested site's two extractors.
func nestedRegistry(site *nestedSite) (*Registry, *httpx.Client) {
	client := httpx.New("test-agent", "en-US", 0, 10*time.Second)
	reg := &Registry{client: client, fallback: NewDirect(client)}
	reg.extractors = []Extractor{
		&nestedExtractor{kind: "root", site: site, client: client, registry: reg},
		&nestedExtractor{kind: "list", site: site, client: client, registry: reg},
	}
	return reg, client
}

// runRoots extracts roots root jobs at once and checks each found every leaf,
// in the listing's order.
func runRoots(t *testing.T, site *nestedSite, reg *Registry, roots int) {
	t.Helper()
	var wg sync.WaitGroup
	for r := range roots {
		wg.Go(func() {
			res, _, err := reg.Extract(context.Background(), fmt.Sprintf("%s/root/r%d", site.srv.URL, r), Options{})
			if err != nil {
				t.Errorf("root %d: %v", r, err)
				return
			}
			if want := site.fanout * site.fanout; len(res.Files) != want {
				t.Errorf("root %d: %d files, want %d", r, len(res.Files), want)
				return
			}
			if first := res.Files[0].Name; first != fmt.Sprintf("r%d-0-0.bin", r) {
				t.Errorf("root %d: first file %q, want the listing's order", r, first)
			}
		})
	}
	wg.Wait()
}

// TestNestedDiscoveryRequestsInFlight measures, at the HTTP boundary, how
// many discovery requests a nested extraction has open at once: each map is
// bounded by config.PageFetchConcurrency, and nesting multiplies the bound,
// so one root reaches PageFetchConcurrency² leaf requests in flight and two
// roots twice that. This is the baseline an extraction-wide admission is
// measured against.
func TestNestedDiscoveryRequestsInFlight(t *testing.T) {
	c := config.PageFetchConcurrency
	for _, roots := range []int{1, 2} {
		t.Run(fmt.Sprint(roots, " roots"), func(t *testing.T) {
			want := int32(roots * c * c)
			site := newNestedSite(t, c, want, time.Second)
			reg, client := nestedRegistry(site)
			defer client.CloseIdleConnections()
			runRoots(t, site, reg, roots)
			// A root probe, c listing probes and c² leaves, per root.
			if total := site.total.Load(); total != int32(roots*(1+c+c*c)) {
				t.Errorf("%d requests, want %d", total, roots*(1+c+c*c))
			}
			t.Logf("%d roots: peak %d leaf requests in flight, %d requests in all", roots, site.peak.Load(), site.total.Load())
			if site.peak.Load() != want {
				t.Errorf("peak %d leaf requests in flight, want %d", site.peak.Load(), want)
			}
		})
	}
}
