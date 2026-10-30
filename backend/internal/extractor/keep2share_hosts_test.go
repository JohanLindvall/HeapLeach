// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// The second host the page names keeps its own API, notes and waits, under
// the one extractor and its one proxy group.
func TestKeep2ShareSecondHostKeepsItsAPIAndRoute(t *testing.T) {
	var calls atomic.Int32
	k := keep2ShareProtocolTestSite(t, "FileBoom", func(w http.ResponseWriter, in map[string]string) {
		if calls.Add(1)%2 == 1 {
			if in["free_download_key"] != "" || in["captcha_response"] == "" {
				t.Error("new route reused another address's ticket")
			}
			fmt.Fprint(w, `{"status":"success","free_download_key":"ticket","time_wait":0}`)
			return
		}
		if in["free_download_key"] != "ticket" {
			t.Error("ticket was not redeemed")
		}
		fmt.Fprintf(w, `{"status":"success","url":"https://storage.example.test/clip?temp_url_expires=%d"}`, time.Now().Add(time.Hour).Unix())
	})
	u, _ := url.Parse("https://files.example.test/file/test-file")
	res, err := k.ex.Extract(keep2SharePageContext(t, u, k.label), u, Options{})
	if err != nil || k.ex.Name() != "keep2share" || len(res.Files) != 1 || *res.Files[0].Pace != (Pace{Files: 1, Streams: 1, Group: "keep2share", PerRoute: true}) {
		t.Fatalf("metadata and pacing: result=%+v error=%v", res, err)
	}
	if calls.Load() != 0 {
		t.Fatal("extraction redeemed a ticket before dispatch")
	}
	var notes []string
	ctx := WithResolveNote(context.Background(), func(note string) { notes = append(notes, note) })
	for _, route := range []string{"one", "one", "two"} {
		if _, err := res.Files[0].Resolve(httpx.WithRoute(ctx, route, k.ex.client, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 4 || len(notes) == 0 || !strings.HasPrefix(notes[0], "FileBoom:") {
		t.Fatalf("route tickets or host notes: calls=%d notes=%v", calls.Load(), notes)
	}
	var wait *WaitError
	if !errors.As(k.wait(ctx, time.Minute), &wait) || !strings.HasPrefix(wait.Reason, "FileBoom:") {
		t.Fatal("cooldown did not name the host")
	}
	if NewKeep2Share(k.ex.client).services[0].waiting(ctx) != nil {
		t.Fatal("one host's wait blocked the other")
	}
}

func TestKeep2ShareAPIErrorsNameTheHostThatAnswered(t *testing.T) {
	k := keep2ShareProtocolTestSite(t, "FileBoom", func(w http.ResponseWriter, _ map[string]string) {
		w.WriteHeader(http.StatusNotAcceptable)
		fmt.Fprint(w, `{"status":"error","errorCode":10,"errors":[{"code":7}]}`)
	})
	_, err := k.call(context.Background(), "getUrl", map[string]string{"file_id": "test-file"})
	var api *keep2ShareResponse
	if !errors.As(err, &api) || !strings.HasPrefix(err.Error(), "fileboom:") || !strings.Contains(err.Error(), "Premium") {
		t.Fatalf("lost the host's restriction: %v", err)
	}
}

// A page that names neither host is not asked of either API.
func TestKeep2ShareExtractNeedsAPageNamingAHost(t *testing.T) {
	k := keep2ShareTestSite(t, func(http.ResponseWriter, map[string]string) { t.Error("asked for a download") })
	u, _ := url.Parse("https://files.example.test/file/test-file")
	if _, err := k.ex.Extract(keep2SharePageContext(t, u, "Elsewhere"), u, Options{}); err == nil || !strings.Contains(err.Error(), "names neither") {
		t.Fatalf("error = %v; want the page refused", err)
	}
}
