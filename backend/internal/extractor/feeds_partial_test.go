// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A plain-text feed is identified from a prefix, but its episodes can run
// beyond that prefix. Both a valid early enclosure and a late first enclosure
// must cause extraction to read the complete feed with its own request bound.
func TestFeedsReadPastAPlainTextProbe(t *testing.T) {
	for _, format := range []struct {
		name, open, close, episode string
	}{
		{
			name:    "RSS",
			open:    `<rss version="2.0"><channel><title>Example Show</title>`,
			close:   `</channel></rss>`,
			episode: `<item><title>%[1]s episode</title><enclosure url="/media/%[1]s.mp3" type="audio/mpeg" length="1"/></item>`,
		},
		{
			name:    "Atom",
			open:    `<feed xmlns="http://www.w3.org/2005/Atom"><title>Example Show</title>`,
			close:   `</feed>`,
			episode: `<entry><title>%[1]s episode</title><link rel="enclosure" href="/media/%[1]s.mp3" type="audio/mpeg" length="1"/></entry>`,
		},
	} {
		for _, placement := range []struct {
			name        string
			firstBefore bool
		}{
			{"enclosure before and after prefix", true},
			{"first enclosure after prefix", false},
		} {
			t.Run(format.name+"/"+placement.name, func(t *testing.T) {
				first := fmt.Sprintf(format.episode, "first")
				padding := "<!--" + strings.Repeat("x", config.ProbeSniffBytes) + "-->"
				doc := format.open
				if placement.firstBefore {
					doc += first + padding
				} else {
					doc += padding + first
				}
				doc += fmt.Sprintf(format.episode, "second") + format.close
				var pageReads, feedReads, mediaReads atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/feed" {
						mediaReads.Add(1)
						http.NotFound(w, r)
						return
					}
					if r.Method != http.MethodGet {
						t.Errorf("feed request method = %s, want GET", r.Method)
					}
					pageReads.Add(1)
					if r.Header.Get(httpx.HeaderAccept) == feedAccept {
						feedReads.Add(1)
					}
					w.Header().Set(httpx.HeaderContentType, "text/plain")
					_, _ = io.WriteString(w, doc)
				}))
				t.Cleanup(srv.Close)
				res, ex, err := NewRegistry(&config.Config{}, testClient()).
					Extract(context.Background(), srv.URL+"/feed", Options{})
				if err != nil {
					t.Errorf("extract: %v", err)
				}
				if ex == nil || ex.Name() != "feed" {
					t.Errorf("extractor = %v, want feed", ex)
				}
				var urls []string
				if res != nil {
					for _, file := range res.Files {
						urls = append(urls, file.URL)
					}
					if res.Note != "" {
						t.Errorf("complete feed has note %q", res.Note)
					}
				}
				// Feeds list newest first; extraction queues oldest first.
				want := []string{srv.URL + "/media/second.mp3", srv.URL + "/media/first.mp3"}
				if !slices.Equal(urls, want) {
					t.Errorf("episode URLs = %v, want %v", urls, want)
				}
				if got := pageReads.Load(); got != 2 {
					t.Errorf("feed GETs = %d, want the prefix probe and complete feed read", got)
				}
				if got := feedReads.Load(); got != 1 {
					t.Errorf("requests with feed Accept = %d, want 1", got)
				}
				if got := mediaReads.Load(); got != 0 {
					t.Errorf("media requests = %d, want none during extraction", got)
				}
			})
		}
	}
}
