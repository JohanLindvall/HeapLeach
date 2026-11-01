// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestChildFileCapContinuesPastUnavailable(t *testing.T) {
	for _, site := range []string{"coomerfans", "erome", "peertube", "bitchute"} {
		for _, later := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/later_page=%v", site, later), func(t *testing.T) {
				ex, input, hits := childFileCapFixture(t, site, later)
				res, err := ex.Extract(t.Context(), input, Options{limits: Limits{Files: 1}})
				if err != nil || len(res.Files) != 1 || res.Files[0].URL != "https://media.example.test/Second.jpg" {
					t.Fatalf("result=%+v, err=%v; want the later available file", res, err)
				}
				pages := 1
				if later {
					pages = 2
				}
				if hits("Second") != 1 || hits("Third") != 0 || hits("page") != pages {
					t.Fatalf("requests: second=%d, third=%d, pages=%d", hits("Second"), hits("Third"), hits("page"))
				}
				if !strings.Contains(res.Note, "file limit") {
					t.Errorf("the unread tail is not reported: %q", res.Note)
				}
			})
		}
	}
}

func TestChildInspectionBudgetStillBoundsEmptySources(t *testing.T) {
	for _, site := range []string{"coomerfans", "erome"} {
		t.Run(site, func(t *testing.T) {
			ex, input, hits := childFileCapFixture(t, site, true)
			res, err := ex.Extract(t.Context(), input, Options{limits: Limits{Files: 1, Sources: 1}})
			if err == nil || res != nil || hits("First") != 1 || hits("Second") != 0 || hits("page") != 1 {
				t.Fatalf("result=%+v err=%v first=%d second=%d pages=%d", res, err, hits("First"), hits("Second"), hits("page"))
			}
		})
	}
}

// The HTTP server is a terminal proxy, including for synthetic links built
// from the two HTML readers' host roots. It never forwards a request.
func childFileCapFixture(t *testing.T, site string, later bool) (Extractor, *url.URL, func(string) int) {
	t.Helper()
	var mu sync.Mutex
	counts := map[string]int{}
	hit := func(key string) { mu.Lock(); counts[key]++; mu.Unlock() }
	coomerRoot := strings.Replace(coomerFansRoot, "https:", "http:", 1)
	eromeRoot := strings.Replace(eromeReferer, "https:", "http:", 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		path := r.URL.Path
		switch {
		case path == "/u/sample/Example/Creator" || path == "/Creator":
			hit("page")
			page := r.URL.Query().Get("page")
			ids := []string{"First", "Second"}
			if later && page == "" {
				ids = []string{"First"}
			} else if later && page == "2" {
				ids = []string{"First", "Second"} // Repeated child costs no request.
			} else if page != "" {
				ids = []string{"Third"}
			}
			fmt.Fprint(w, `<html><h1>Creator</h1>`)
			for _, id := range ids {
				link := eromeRoot + "/a/" + id
				if site == "coomerfans" {
					link = coomerRoot + "/p/" + id + "/Example/sample"
				}
				fmt.Fprintf(w, `<a href="%s">%s</a>`, link, id)
			}
			fmt.Fprint(w, `<a class="next" href="?page=2">Next</a></html>`)
		case strings.HasPrefix(path, "/p/") || strings.HasPrefix(path, "/a/"):
			id := strings.Split(path, "/")[2]
			hit(id)
			if site == "erome" && id == "First" {
				http.Error(w, "Gone", http.StatusGone)
				return
			}
			if site == "coomerfans" {
				fmt.Fprint(w, `<h1>Post</h1><div class="post-body">Text`)
				if id != "First" {
					fmt.Fprintf(w, `<img src="https://media.example.test/%s.jpg">`, id)
				}
			} else {
				fmt.Fprintf(w, `<h1 class="album-title-page">Album</h1><div class="media-group"><img data-src="https://media.example.test/%s.jpg">`, id)
			}
			fmt.Fprint(w, `</div>`)
		case path == "/api/v1/video-channels/synthetic/videos" || path == "/channel/videos":
			hit("page")
			start := r.URL.Query().Get("start")
			if site == "bitchute" {
				var in bitchuteChannelRequest
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					t.Error(err)
				}
				start = strconv.Itoa(in.Offset)
			}
			ids := []string{"First", "Second", "Third"}
			if later && start == "0" {
				ids = []string{"First"}
				if site == "bitchute" {
					for n := 1; n < bitchutePageSize; n++ {
						ids = append(ids, fmt.Sprintf("First%d", n))
					}
				}
			} else if later {
				ids = []string{"Second", "Third"}
			}
			var rows []map[string]string
			for _, id := range ids {
				rows = append(rows, map[string]string{"uuid": id, "video_id": id, "video_name": id})
			}
			w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
			_ = json.NewEncoder(w).Encode(map[string]any{"total": 3, "data": rows, "videos": rows})
		case path == "/api/v1/video-channels/synthetic" || path == "/channel":
			fmt.Fprint(w, `{"displayName":"Channel","channel_name":"Channel"}`)
		case strings.HasPrefix(path, "/api/v1/videos/") || path == "/video/media":
			id := strings.TrimPrefix(path, "/api/v1/videos/")
			if site == "bitchute" {
				var in bitchuteVideoRequest
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					t.Error(err)
				}
				id = in.VideoID
			}
			hit(id)
			if strings.HasPrefix(id, "First") {
				fmt.Fprint(w, `{"name":"Unavailable","isLive":true,"files":[]}`)
			} else {
				fmt.Fprintf(w, `{"name":"%[1]s","media_url":"https://media.example.test/%[1]s.jpg","files":[{"fileUrl":"https://media.example.test/%[1]s.jpg","size":1234,"resolution":{"id":720}}]}`, id)
			}
		case strings.HasPrefix(path, "/embed/First"):
			fmt.Fprint(w, `<html>Unavailable</html>`)
		default:
			t.Errorf("unexpected fixture request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := testClient().ThroughProxy(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	var ex Extractor
	input := "http://listing.example.test"
	switch site {
	case "coomerfans":
		ex, input = NewCoomerFans(client), input+"/u/sample/Example/Creator"
	case "erome":
		ex, input = NewErome(client), input+"/Creator"
	case "peertube":
		ex, input = NewPeerTube(client), input+"/c/synthetic"
	case "bitchute":
		b := NewBitChute(client)
		b.api, b.site = "http://api.example.test", "http://video.example.test"
		ex, input = b, input+"/channel/synthetic"
	}
	return ex, mustParse(t, input), func(key string) int { mu.Lock(); defer mu.Unlock(); return counts[key] }
}

func TestListingFilesKeepsOrderCapsAndCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			f := listingFiles[int]{limit: 3}
			done, cut := f.add(ctx, []int{0, 1, 2, 3}, func(_ context.Context, id int) ([]File, error) {
				if id == 0 {
					return nil, nil
				}
				if cancel {
					stop()
				}
				return []File{{Name: fmt.Sprint(id, "a"), Headers: httpx.Referer("https://example.test/")}, {Name: fmt.Sprint(id, "b")}}, nil
			})
			files, _, err := f.result(ctx, "fixture", "posts", "listing")
			if cancel {
				if !errors.Is(err, context.Canceled) || files != nil {
					t.Fatalf("files=%+v err=%v", files, err)
				}
				return
			}
			if !done || !cut || err != nil || len(files) != 3 || files[0].Name != "1a" || files[2].Name != "2a" || files[0].Headers[httpx.HeaderReferer] == "" {
				t.Fatalf("done=%v cut=%v files=%+v err=%v", done, cut, files, err)
			}
		})
	}
}

func TestChannelInspectionLimitsAreIndependentOfFiles(t *testing.T) {
	for _, site := range []string{"peertube", "bitchute"} {
		t.Run(site, func(t *testing.T) {
			limit := config.MaxListingVideos
			if site == "bitchute" {
				limit = config.MaxChannelVideos
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var rows []map[string]string
				for n := 0; n <= limit; n++ {
					rows = append(rows, map[string]string{"uuid": strconv.Itoa(n), "video_id": strconv.Itoa(n)})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"total": limit + 1, "data": rows, "videos": rows})
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			seen := 0
			var note string
			var err error
			if site == "peertube" {
				_, note, err = peerTubeListVideos(t.Context(), client, srv.URL, nil, "channel", func(ids []string) (bool, bool) { seen += len(ids); return false, false })
			} else {
				b := NewBitChute(client)
				b.api = srv.URL
				_, note, err = b.channelVideos(t.Context(), "synthetic", func(videos []bitchuteVideo) (bool, bool) { seen += len(videos); return false, false })
			}
			if err != nil || seen != limit || note != listingLimitNote {
				t.Fatalf("inspected=%d note=%q err=%v; want budget %d", seen, note, err, limit)
			}
		})
	}
}
