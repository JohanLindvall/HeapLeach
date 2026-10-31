// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestWordPressExtractionUsesManagerLogLevelAndJob(t *testing.T) {
	srv := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/category/example/":
			fmt.Fprint(w, `<body class="archive category"><main><article class="hentry"><a rel="bookmark" href="/post/">Post</a></article></main>`)
		case "/post/":
			fmt.Fprint(w, `<main><article><h1 class="entry-title">Post</h1><div class="entry-content"><img src="/photo.jpg"></div></article></main>`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo} {
		t.Run(level.String(), func(t *testing.T) {
			m := busyManager(t)
			var output bytes.Buffer
			m.log = slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: level}))
			res, _, err := m.extractSource(context.Background(), &Job{ID: "offline-job", Source: srv.URL + "/category/example/"})
			if err != nil || res == nil || len(res.Files) != 1 {
				t.Fatalf("extraction result=%+v err=%v", res, err)
			}
			if level == slog.LevelInfo {
				if output.Len() != 0 {
					t.Fatalf("debug logs appeared at the normal level: %s", output.String())
				}
			} else if !strings.Contains(output.String(), "job=offline-job") || !strings.Contains(output.String(), "wordpress: extraction complete") {
				t.Fatalf("manager logger did not reach extraction: %s", output.String())
			}
		})
	}
}
