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

func TestFileBoomFreeProtocolKeepsItsHostAndRoute(t *testing.T) {
	var calls atomic.Int32
	k := keep2ShareProtocolTestSite(t, NewFileBoom, func(w http.ResponseWriter, in map[string]string) {
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
	res, err := k.Extract(context.Background(), u, Options{})
	if err != nil || k.Name() != "fileboom" || len(res.Files) != 1 || *res.Files[0].Pace != (Pace{Files: 1, Streams: 1, Group: "fileboom", PerRoute: true}) {
		t.Fatalf("FileBoom metadata and pacing: result=%+v error=%v", res, err)
	}
	if calls.Load() != 0 {
		t.Fatal("extraction redeemed a ticket before dispatch")
	}
	var notes []string
	ctx := WithResolveNote(context.Background(), func(note string) { notes = append(notes, note) })
	for _, route := range []string{"one", "one", "two"} {
		if _, err := res.Files[0].Resolve(httpx.WithRoute(ctx, route, k.client, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 4 || len(notes) == 0 || !strings.HasPrefix(notes[0], "FileBoom:") {
		t.Fatalf("route tickets or host notes: calls=%d notes=%v", calls.Load(), notes)
	}
	var wait *WaitError
	if !errors.As(k.wait(ctx, time.Minute), &wait) || !strings.HasPrefix(wait.Reason, "FileBoom:") {
		t.Fatal("cooldown did not name FileBoom")
	}
	if NewKeep2Share(k.client).waiting(ctx) != nil {
		t.Fatal("FileBoom's wait blocked K2S")
	}
}

func TestFileBoomAPIErrorsNameTheCorrectService(t *testing.T) {
	k := keep2ShareProtocolTestSite(t, NewFileBoom, func(w http.ResponseWriter, _ map[string]string) {
		w.WriteHeader(http.StatusNotAcceptable)
		fmt.Fprint(w, `{"status":"error","errorCode":10,"errors":[{"code":7}]}`)
	})
	_, err := k.call(context.Background(), "getUrl", map[string]string{"file_id": "test-file"})
	var api *keep2ShareResponse
	if !errors.As(err, &api) || !strings.HasPrefix(err.Error(), "fileboom:") || !strings.Contains(err.Error(), "Premium") {
		t.Fatalf("lost FileBoom's restriction: %v", err)
	}
}
