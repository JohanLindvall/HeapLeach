// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestFediverseDiscoveryRecoversTransientFailures(t *testing.T) {
	for _, endpoint := range []string{"index", "document"} {
		for failures := 0; failures <= 2; failures++ {
			t.Run(fmt.Sprintf("%s/failures=%d", endpoint, failures), func(t *testing.T) {
				var pages, indexes, documents, statuses, other atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/@alice/123":
						pages.Add(1)
						w.Header().Set("Content-Type", "text/html")
						fmt.Fprint(w, `<html><head><title>Synthetic App</title></head><body><div id="app"></div></body></html>`)
					case "/.well-known/nodeinfo":
						n := indexes.Add(1)
						if endpoint == "index" && n <= int32(failures) {
							http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
							return
						}
						fmt.Fprintf(w, `{"links":[{"rel":"http://nodeinfo.diaspora.software/ns/schema/2.0","href":"http://%s/nodeinfo/2.0"}]}`, r.Host)
					case "/nodeinfo/2.0":
						n := documents.Add(1)
						if endpoint == "document" && n <= int32(failures) {
							http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
							return
						}
						fmt.Fprint(w, `{"version":"2.0","software":{"name":"mastodon","version":"1.0.0"},"protocols":["activitypub"],"services":{"inbound":[],"outbound":[]},"openRegistrations":false,"usage":{"users":{"total":1},"localPosts":1,"localComments":0},"metadata":{}}`)
					case "/api/v1/statuses/123":
						statuses.Add(1)
						fmt.Fprint(w, `{"id":"123","account":{"id":"42","username":"alice","acct":"alice"},"media_attachments":[{"url":"https://media.example.test/synthetic-image.jpg"}]}`)
					default:
						other.Add(1)
						http.Error(w, "unknown endpoint", http.StatusNotFound)
					}
				}))
				defer srv.Close()
				client := httpx.New("audit", "en-US", 3, 10*time.Second)
				defer client.CloseIdleConnections()
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				res, ex, err := NewRegistry(&config.Config{}, client).Extract(ctx, srv.URL+"/@alice/123", Options{})
				if ex.Name() != "fediverse" || statuses.Load() != 1 || other.Load() != 0 {
					t.Errorf("extractor=%v status reads=%d unexpected reads=%d", ex, statuses.Load(), other.Load())
				}
				wantIndexes, wantDocuments := int32(1), int32(1)
				if endpoint == "index" {
					wantIndexes += int32(failures)
				} else {
					wantDocuments += int32(failures)
				}
				if indexes.Load() != wantIndexes || documents.Load() != wantDocuments {
					t.Errorf("discovery index/document GETs = %d/%d, want %d/%d", indexes.Load(), documents.Load(), wantIndexes, wantDocuments)
				}
				if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != "https://media.example.test/synthetic-image.jpg" {
					t.Fatal("the available status was not extracted after transient discovery errors")
				}
			})
		}
	}
}

func TestFediverseRecoveryPreservesFallbackAndDefiniteAnswers(t *testing.T) {
	for _, tc := range []struct {
		name, path, page, metadata string
		status, requests           int
		wantFile                   bool
	}{
		{"generic media", "/@alice/123", `<html><video src="https://media.example.test/synthetic.mp4"></video></html>`, "busy", 503, 2, true},
		{"unrelated form", "/article/synthetic", `<html><body>Empty article</body></html>`, "busy", 503, 0, false},
		{"missing endpoint", "/@alice/123", `<html><body>App</body></html>`, "missing", 404, 1, false},
		{"unrelated json", "/@alice/123", `<html><body>App</body></html>`, `{"message":"not a platform"}`, 200, 1, false},
		{"invalid json", "/@alice/123", `<html><body>App</body></html>`, `{`, 200, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var discovery atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case tc.path:
					w.Header().Set(httpx.HeaderContentType, "text/html")
					fmt.Fprint(w, tc.page)
				case fediWellKnown:
					discovery.Add(1)
					w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.metadata)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			res, ex, err := NewRegistry(&config.Config{}, client).Extract(context.Background(), srv.URL+tc.path, Options{})
			if tc.wantFile {
				if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != "https://media.example.test/synthetic.mp4" {
					t.Fatalf("generic media lost: %v, %v", res, err)
				}
			} else if !errors.Is(err, errPlainPage) {
				t.Fatalf("got %v, want original plain-page refusal", err)
			}
			if ex.Name() != "direct" || int(discovery.Load()) != tc.requests {
				t.Fatalf("extractor=%s discovery requests=%d, want direct/%d", ex.Name(), discovery.Load(), tc.requests)
			}
		})
	}
}

func TestFediverseDiscoveryRecoveryExhaustion(t *testing.T) {
	var discovery atomic.Int32
	var available atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/@alice/123":
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprint(w, `<html><body>App</body></html>`)
		case fediWellKnown:
			discovery.Add(1)
			if !available.Load() {
				http.Error(w, "temporarily busy", http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, fmt.Sprintf(`{"links":[{"rel":%q,"href":"http://%s/nodeinfo/2.0"}]}`, fediSchemaRel+"2.0", r.Host))
		case "/nodeinfo/2.0":
			writeJSON(w, `{"software":{"name":"mastodon"}}`)
		case "/api/v1/statuses/123":
			writeJSON(w, `{"id":"123","account":{"acct":"alice","username":"alice"},"media_attachments":[{"url":"https://media.example.test/synthetic.jpg"}]}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := httpx.New("test", "en-US", 1, time.Second)
	defer client.CloseIdleConnections()
	reg := NewRegistry(&config.Config{}, client)
	_, ex, err := reg.Extract(context.Background(), srv.URL+"/@alice/123", Options{})
	status, ok := errors.AsType[*httpx.StatusError](err)
	if !ok || status.Code != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "temporarily busy") ||
		strings.Contains(err.Error(), "not a fediverse") || ex.Name() != "fediverse" {
		t.Fatalf("lost discovery failure: extractor=%v error=%v", ex, err)
	}
	if got, want := int(discovery.Load()), config.PlatformProbeAttempts+2; got != want {
		t.Fatalf("discovery attempts=%d, want probe budget plus two normal attempts (%d)", got, want)
	}
	available.Store(true)
	res, ex, err := reg.Extract(context.Background(), srv.URL+"/@alice/123", Options{})
	if err != nil || res == nil || len(res.Files) != 1 || ex.Name() != "fediverse" {
		t.Fatalf("transient failure survived into next submission: %v, %v", res, err)
	}
}

func TestFediverseRecoveryKeepsAcquisitionIdentity(t *testing.T) {
	for _, changed := range []string{"client", "route", "canceled", "unasked"} {
		t.Run(changed, func(t *testing.T) {
			var discovery atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == fediWellKnown {
					discovery.Add(1)
					http.Error(w, "busy", http.StatusServiceUnavailable)
					return
				}
				w.Header().Set(httpx.HeaderContentType, "text/html")
				fmt.Fprint(w, `<html><body>App</body></html>`)
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			reader := NewFediverse(client)
			u := mustParse(t, srv.URL+"/@alice/123")
			ctx := context.Background()
			p := probePage(ctx, client, u)
			if changed != "unasked" && reader.Recognise(ctx, p) {
				t.Fatal("failed discovery identified a platform")
			}
			before := discovery.Load()
			switch changed {
			case "client":
				other := httpx.New("another-agent", "en-US", 0, time.Second)
				defer other.CloseIdleConnections()
				reader = NewFediverse(other)
			case "route":
				ctx = httpx.WithRoute(ctx, "another-route", client, nil)
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			identified, err := reader.recoverRecognition(ctx, p)
			if identified || discovery.Load() != before || (changed == "canceled") != errors.Is(err, context.Canceled) {
				t.Fatalf("recovery crossed %s: identified=%v requests=%d -> %d error=%v", changed, identified, before, discovery.Load(), err)
			}
		})
	}
}

func TestFediverseFollowedPostRecoversDiscovery(t *testing.T) {
	var documents atomic.Int32
	var indexes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/@alice/123":
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprint(w, `<html><body>App</body></html>`)
		case fediWellKnown:
			if indexes.Add(1) != 1 {
				http.Error(w, "index already consumed", http.StatusGone)
				return
			}
			writeJSON(w, fmt.Sprintf(`{"links":[{"rel":%q,"href":"http://%s/nodeinfo/2.0"}]}`, fediSchemaRel+"2.0", r.Host))
		case "/nodeinfo/2.0":
			if documents.Add(1) <= config.PlatformProbeAttempts {
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, `{"software":{"name":"mastodon"}}`)
		case "/api/v1/statuses/123":
			writeJSON(w, `{"id":"123","account":{"acct":"alice","username":"alice"},"media_attachments":[{"url":"https://media.example.test/synthetic.jpg"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	reg := NewRegistry(&config.Config{}, client)
	res, ex, err := reg.Extract(context.Background(), srv.URL+"/@alice/123", Options{strict: true})
	if err != nil || res == nil || len(res.Files) != 1 || ex.Name() != "fediverse" || indexes.Load() != 1 || documents.Load() != config.PlatformProbeAttempts+1 {
		t.Fatalf("followed post recovery: result=%v extractor=%v error=%v index/document reads=%d/%d", res, ex, err, indexes.Load(), documents.Load())
	}
}
