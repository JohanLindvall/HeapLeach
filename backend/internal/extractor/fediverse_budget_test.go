// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Every status has a fresh id, but all statuses on a page carry the same
// attachment. The post cursor must advance even when the file budget does not.
func mastodonBudgetPage(page, count int, media string) string {
	items := make([]string, 0, count)
	for i := range count {
		id := 1_000_000 - page*fediMastodonPageSize - i
		items = append(items, fmt.Sprintf(`{"id":"%d","account":{"id":"42","acct":"alice"},
			"media_attachments":[{"id":"attachment-%d","type":"image","url":%q}]}`,
			id, id, media))
	}
	return "[" + strings.Join(items, ",") + "]"
}

func TestFediverseFileBudgetCountsUniqueMediaAcrossPages(t *testing.T) {
	const shared = "https://files.example.test/media/shared-picture.jpg"
	const later = "https://files.example.test/media/later-picture.jpg"
	for _, tc := range []struct {
		name       string
		uniquePage int
		lastShort  bool
		wantPages  int
		wantFiles  []string
		wantNote   string
	}{
		{
			name:       "two full pages repeat media before a new file",
			uniquePage: 3, wantPages: 3,
			wantFiles: []string{shared, later}, wantNote: "partial — file limit",
		},
		{
			name:       "new media on each page fills the actual budget",
			uniquePage: 2, wantPages: 2,
			wantFiles: []string{shared, later}, wantNote: "partial — file limit",
		},
		{
			name:       "short last page fills the budget without a cut",
			uniquePage: 3, lastShort: true, wantPages: 3,
			wantFiles: []string{shared, later},
		},
		{
			name:      "new posts with duplicate media reach the page bound",
			wantPages: config.MaxTimelinePages,
			wantFiles: []string{shared}, wantNote: "partial — page limit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pages atomic.Int32
			srv := fediServer(t, "mastodon", func(w http.ResponseWriter, r *http.Request) bool {
				switch r.URL.Path {
				case "/api/v1/accounts/lookup":
					writeJSON(w, `{"id":"42","username":"alice","acct":"alice"}`)
				case "/api/v1/accounts/42/statuses":
					page := int(pages.Add(1))
					media, count := shared, fediMastodonPageSize
					if page == tc.uniquePage {
						media = later
						if tc.lastShort {
							count = 1
						}
					}
					writeJSON(w, mastodonBudgetPage(page, count, media))
				default:
					return false
				}
				return true
			})
			f := NewFediverse(testClient())
			res, err := f.Extract(context.Background(), mustParse(t, srv.URL+"/@alice"), Options{limits: Limits{Files: 2}})
			if err != nil {
				t.Fatal(err)
			}
			var files []string
			for _, file := range res.Files {
				files = append(files, file.URL)
			}
			if !slices.Equal(files, tc.wantFiles) {
				t.Errorf("files = %v, want unique media in first-seen order %v", files, tc.wantFiles)
			}
			if got := int(pages.Load()); got != tc.wantPages {
				t.Errorf("requested %d timeline pages, want %d", got, tc.wantPages)
			}
			if res.Note != tc.wantNote {
				t.Errorf("note = %q, want %q", res.Note, tc.wantNote)
			}
		})
	}
}
