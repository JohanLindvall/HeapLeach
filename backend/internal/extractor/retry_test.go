// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A player page served once with its rendition list empty, though it says
// the video is fine, and playable from then on, is read again by the retry:
// through the registry, whose probe already holds the empty page, the
// retry asks the site rather than being handed the probe's answer again.
func TestRetryOfAProbedPageReadsItAfresh(t *testing.T) {
	empty := strings.Replace(okruEmbedPage, `\"videos\":[{\"name\":\"sd\",\"url\":\"https://vd1.example.test/sd?expires=1\"},{\"name\":\"full\",\"url\":\"https://vd1.example.test/full?expires=1\"},{\"name\":\"mobile\",\"url\":\"https://vd1.example.test/mobile?expires=1\"}],\"hlsManifestUrl\":\"https://vd1.example.test/video.m3u8\"`, `\"videos\":[]`, 1)
	if empty == okruEmbedPage {
		t.Fatal("fixture did not change")
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		if requests.Add(1) == 1 {
			_, _ = io.WriteString(w, empty)
			return
		}
		_, _ = io.WriteString(w, okruEmbedPage)
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()

	// The row as registered, but for a page that stays on the local server.
	row := okruPlayer
	row.page = func(u *url.URL) (string, error) { return u.String(), nil }
	player := NewPlayer(client, &row)
	reg := &Registry{client: client, extractors: []Extractor{player}, fallback: NewDirect(client)}

	res, ex, err := reg.Extract(context.Background(), srv.URL+"/videoembed/1234", Options{})
	if err != nil || ex != player || len(res.Files) != 1 {
		t.Fatalf("result=%+v extractor=%v err=%v", res, ex, err)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("%d requests, want the probe and one fresh read", n)
	}
}

// freshRead withholds the probed page and nothing else.
func TestFreshReadWithholdsOnlyTheProbe(t *testing.T) {
	type other struct{}
	u, _ := url.Parse("https://example.test/a")
	p := &Page{URL: u, Final: u, Status: http.StatusOK, Body: "<html></html>"}
	ctx := context.WithValue(withPage(context.Background(), p), other{}, "kept")
	fresh := freshRead(ctx)
	if probed(fresh, u) != nil {
		t.Error("the probed page reached a fresh read")
	}
	if probed(ctx, u) != p {
		t.Error("the original context lost its page")
	}
	if fresh.Value(other{}) != "kept" {
		t.Error("a fresh read lost the rest of the context")
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if freshRead(ctx).Err() == nil {
		t.Error("a fresh read outlived its caller's cancellation")
	}
}
