// SPDX-License-Identifier: MIT

package extractor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestDoodDiagnosticsDistinguishBlockedAndMissingPlayers(t *testing.T) {
	for _, tc := range []struct {
		name, stage, body, kind string
		status                  int
		header, challenge, ok   bool
	}{
		{name: "missing token", stage: "player", body: `<title>Layout changed</title><p>private-page-body</p><script>turnstile.render(document.body, {size: 'invisible'});</script>`, kind: "text/html", status: 200},
		{name: "unexpected content type", stage: "player", body: "private-page-body", kind: "text/plain", status: 200},
		{name: "interstitial", stage: "player", body: doodInterstitialFixture, kind: "text/html", status: 200, challenge: true},
		{name: "header challenge", stage: "player", body: "private-page-body", kind: "text/html", status: 403, header: true, challenge: true},
		{name: "token gate", stage: "token", body: doodGateFixture, kind: "text/html", status: 200, challenge: true},
		{name: "playable", stage: "token", body: "https://media.example.test/private-media-signature/", kind: "text/plain", status: 200, ok: true},
	} {
		for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo} {
			t.Run(tc.name+"/"+level.String(), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/e/alias" {
						http.Redirect(w, r, "/e/synthetic", http.StatusFound)
						return
					}
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "private-cookie-value", Path: "/"})
					if tc.stage == "token" && r.URL.Path == "/e/synthetic" {
						fmt.Fprint(w, `<title>Test Clip</title><script>fetch('/pass_md5/session/synthetic')</script>`)
						return
					}
					w.Header().Set(httpx.HeaderContentType, tc.kind)
					w.Header().Set("CF-Ray", "synthetic-ray")
					if tc.header {
						w.Header().Set(httpx.HeaderCFMitigated, "challenge")
					}
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				}))
				defer srv.Close()
				client := httpx.New("test", "en", 0, time.Second)
				defer client.CloseIdleConnections()
				var output bytes.Buffer
				log := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: level})).With("job", "test-job")
				ctx := httpx.WithRoute(WithLogger(context.Background(), log), "test-route", client, nil)
				res, err := NewDoodStream(client).Extract(ctx, mustParse(t, srv.URL+"/e/alias"), Options{})
				_, challenged := errors.AsType[*httpx.ChallengeError](err)
				if challenged != tc.challenge || (err == nil) != tc.ok || (res != nil) != tc.ok {
					t.Fatalf("result=%+v error=%v", res, err)
				}
				if level == slog.LevelInfo {
					if output.Len() != 0 {
						t.Fatal("response diagnostics ignored the logger's level")
					}
					return
				}
				text := output.String()
				for _, secret := range []string{"private-page-body", "private-cookie-value", "private-media-signature", "/pass_md5/"} {
					// A failed request's existing error includes its URL; successful
					// responses must never dump token paths or signed media prefixes.
					if secret == "/pass_md5/" && !tc.ok {
						continue
					}
					if strings.Contains(text, secret) {
						t.Fatalf("diagnostics exposed %q", secret)
					}
				}
				seenResponse, seenPage, seenFailure := false, false, false
				for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
					var record map[string]any
					if err := json.Unmarshal([]byte(line), &record); err != nil {
						t.Fatal(err)
					}
					if record["job"] != "test-job" || record["route"] != "test-route" || record["attempt"] != float64(1) {
						t.Fatalf("diagnostics lost extraction context: %v", record)
					}
					if record["stage"] != tc.stage {
						continue
					}
					switch record["msg"] {
					case "http: response":
						seenResponse = true
						if record["status"] != float64(tc.status) || record["content_type"] != tc.kind || record["cf_ray"] != "synthetic-ray" {
							t.Fatalf("missing response evidence: %v", record)
						}
					case "doodstream: page inspected":
						seenPage = true
						if record["challenge"] != tc.challenge || record["body_bytes"] != float64(len(tc.body)) {
							t.Fatalf("wrong body diagnosis: %v", record)
						}
						if tc.stage == "player" && !tc.challenge &&
							(record["redirected"] != true || record["final_page"] != srv.URL+"/e/synthetic") {
							t.Fatalf("missing redirect destination: %v", record)
						}
					case "doodstream: request failed":
						seenFailure = true
						if record["challenge"] != tc.challenge {
							t.Fatalf("lost challenge diagnosis: %v", record)
						}
					}
				}
				if !seenResponse || seenPage == tc.header || seenFailure != tc.challenge {
					t.Fatalf("missing diagnostics: %s", text)
				}
			})
		}
	}
}
