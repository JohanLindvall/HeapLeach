// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A silent proxy can block any network step around OCR. The note must name
// that step, and its timeout must reach route scoring rather than looking
// like either a local reader failure or a user cancellation.
func TestKeep2ShareCaptchaNetworkStallsNameTheStepAndPenaliseTheRoute(t *testing.T) {
	for _, tc := range []struct{ step, note string }{
		{"request", "requesting CAPTCHA"},
		{"image", "downloading CAPTCHA image"},
		{"submit", "submitting CAPTCHA answer"},
		{"redeem", "requesting download link"},
	} {
		t.Run(tc.step, func(t *testing.T) {
			var current atomic.Value
			current.Store("")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var in map[string]string
				_ = json.Unmarshal(body, &in)
				step := ""
				switch r.URL.Path {
				case "/api/v2/requestCaptcha":
					step = "request"
				case "/captcha":
					step = "image"
				case "/api/v2/getUrl":
					step = "submit"
					if in["free_download_key"] != "" {
						step = "redeem"
					}
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if step == tc.step {
					if note := current.Load().(string); !strings.Contains(note, tc.note) {
						t.Errorf("note while %s is blocked = %q, want %q", step, note, tc.note)
					}
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				switch step {
				case "request":
					fmt.Fprint(w, `{"status":"success","challenge":"synthetic-challenge","captcha_url":"http://service.example.test/captcha"}`)
				case "image":
					fmt.Fprint(w, "synthetic image for the fixture solver")
				case "submit":
					fmt.Fprint(w, `{"status":"success","free_download_key":"synthetic-ticket","time_wait":0}`)
				}
			}))
			defer srv.Close()
			client := httpx.New("test", "en", 0, 100*time.Millisecond)
			route, err := client.ThroughProxy(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer route.CloseIdleConnections()
			k := NewKeep2Share(client)
			k.api = "http://service.example.test/api/v2"
			k.solver = func(context.Context, []byte) ([]string, error) { return []string{"aB3dE7"}, nil }
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ctx = WithResolveNote(ctx, func(note string) { current.Store(note) })
			var failures int
			ctx = httpx.WithRoute(ctx, "synthetic-route", route, func(o httpx.RouteObservation) {
				if o.Err != nil {
					failures++
				}
			})
			d := &keep2ShareDownload{host: k, id: "synthetic-file"}
			_, err = d.resolve(ctx)
			if _, ok := errors.AsType[*httpx.RouteError](err); !ok || ctx.Err() != nil || failures != 1 {
				t.Fatalf("error=%v caller=%v route failures=%d; want one penalised network timeout", err, ctx.Err(), failures)
			}
		})
	}
}
