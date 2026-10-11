// SPDX-License-Identifier: MIT

package extractor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type wordPressLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *wordPressLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *wordPressLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type wordPressLogSource struct {
	hostSet
	release <-chan struct{}
	err     error
}

func (*wordPressLogSource) Name() string { return "offline-player" }

func (s *wordPressLogSource) Extract(ctx context.Context, _ *url.URL, _ Options) (*Result, error) {
	ResolveNote(ctx, "Player: requesting token — connection attempt 34")
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	return &Result{Title: "Clip", Files: []File{{Name: "clip.mp4", URL: "https://media.example.test/clip.mp4", Size: -1}}}, nil
}

func wordPressLogRecord(t *testing.T, text, message string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == message {
			return record
		}
	}
	t.Fatalf("missing log %q in:\n%s", message, text)
	return nil
}

func TestWordPressDebugLogsWhileLinkedSourceIsWaiting(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "resolved"
		if fail {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			reg, _, base, asked := wordPressTestSite(t, map[string]string{
				"/category/example/":        wordPressIndexHTML("/category/example/page/2/", "/first/", "/missing/"),
				"/category/example/page/2/": wordPressIndexHTML("", "/second/"),
				"/first/":                   wordPressPostHTML("First", `<iframe src="https://player.example.test/e/clip"></iframe>`),
				"/second/":                  wordPressPostHTML("Second", `<img src="/photo.jpg">`),
			})
			release := make(chan struct{})
			source := &wordPressLogSource{hostSet: hostSet{"player.example.test"}, release: release}
			if fail {
				source.err = errors.New("synthetic player failure")
			}
			reg.extractors = append(reg.extractors, source)
			var output wordPressLogBuffer
			log := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})).With("job", "offline-job")
			ctx, cancel := context.WithTimeout(WithLogger(context.Background(), log), 5*time.Second)
			defer cancel()
			progress := make(chan string, 1)
			ctx = WithResolveNote(ctx, func(note string) { progress <- note })
			type outcome struct {
				res *Result
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				res, _, err := reg.Extract(ctx, base+"/category/example/", Options{})
				done <- outcome{res, err}
			}()
			select {
			case note := <-progress:
				if note != "Player: requesting token — connection attempt 34" {
					t.Fatalf("existing progress callback changed: %q", note)
				}
			case <-ctx.Done():
				t.Fatal("did not reach the linked source")
			}
			select {
			case <-done:
				t.Fatal("extraction finished before the linked source was released")
			default:
			}
			// These records must exist while extraction is still blocked, not
			// merely after it returns and the queue can finally be populated.
			text := output.String()
			listing := wordPressLogRecord(t, text, "wordpress: category listing finished")
			if listing["posts"] != float64(3) {
				t.Fatalf("listing did not report all posts: %v", listing)
			}
			wordPressLogRecord(t, text, "wordpress: following next page")
			stage := wordPressLogRecord(t, text, "wordpress: linked source progress")
			for key, want := range map[string]string{
				"job": "offline-job", "category": base + "/category/example/", "post": base + "/first/",
				"source": "https://player.example.test/e/clip", "extractor": "offline-player",
				"stage": "Player: requesting token — connection attempt 34", "level": "DEBUG",
			} {
				if stage[key] != want {
					t.Errorf("source progress %s=%v, want %q", key, stage[key], want)
				}
			}
			close(release)
			var got outcome
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("extraction did not finish after releasing the source")
			}
			wantFiles := 2
			if fail {
				wantFiles = 1
			}
			if got.err != nil || got.res == nil || len(got.res.Files) != wantFiles {
				t.Fatalf("logging changed extraction: result=%+v err=%v", got.res, got.err)
			}
			text = output.String()
			wordPressLogRecord(t, text, "wordpress: post failed")
			linked := wordPressLogRecord(t, text, "wordpress: linked source "+name)
			if fail && linked["error"] != "synthetic player failure" {
				t.Fatalf("lost the failed source's cause: %v", linked)
			}
			complete := wordPressLogRecord(t, text, "wordpress: extraction complete")
			if complete["files"] != float64(wantFiles) || complete["note"] == "" {
				t.Fatalf("missing final file count or partial result: %v", complete)
			}
			if asked()["/photo.jpg"] != 0 {
				t.Fatal("debug logging fetched media bytes")
			}
		})
	}
}
