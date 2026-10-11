// SPDX-License-Identifier: MIT

package logstore

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

func TestFormattedRecordsKeepLevelsGroupsAndEscapedMessages(t *testing.T) {
	buffer := New()
	log := slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug})).
		With("job", "synthetic-job").WithGroup("player").With("attempt", 2)
	log.Debug("request\nstarted", "error", fmt.Errorf("example failure"))
	log.Warn("challenge", "status", 200)
	page := buffer.Snapshot("", 0)
	if !page.Reset || len(page.Entries) != 2 || page.Entries[0].Level != "DEBUG" || page.Entries[1].Level != "WARN" {
		t.Fatalf("page=%+v", page)
	}
	first := page.Entries[0]
	if _, err := time.Parse(time.RFC3339Nano, first.Time); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`msg="request\nstarted"`, `job=synthetic-job`, `player.attempt=2`, `player.error="example failure"`} {
		if !strings.Contains(first.Text, want) {
			t.Errorf("missing %q in %q", want, first.Text)
		}
	}
	if strings.Contains(first.Text, "\n") {
		t.Fatal("one record was split across lines")
	}
	quiet := slog.New(slog.NewTextHandler(buffer, nil))
	quiet.Debug("must not be captured")
	if got := buffer.Snapshot(page.Session, page.Next); got.Reset || len(got.Entries) != 0 {
		t.Fatalf("disabled message reached the UI: %+v", got)
	}
	// The standard handler may be handed a record without a timestamp.
	if err := log.Handler().Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelInfo, "untimed", 0)); err != nil {
		t.Fatal(err)
	}
	last := buffer.Snapshot(page.Session, page.Next).Entries[0]
	if last.Time != "" || last.Level != "INFO" || !strings.HasPrefix(last.Text, "msg=untimed") {
		t.Fatalf("zero-time record=%+v", last)
	}
}

func TestCursorRecoveryAfterRotationAndRestart(t *testing.T) {
	buffer := New()
	log := slog.New(slog.NewTextHandler(buffer, nil))
	log.Info("first")
	initial := buffer.Snapshot("", 0)
	log.Info("second")
	delta := buffer.Snapshot(initial.Session, initial.Next)
	if delta.Reset || len(delta.Entries) != 1 || delta.Entries[0].ID != 2 {
		t.Fatalf("delta=%+v", delta)
	}
	for i := range config.ServiceLogEntries {
		log.Info("later", "index", i)
	}
	rotated := buffer.Snapshot(initial.Session, initial.Next)
	if !rotated.Reset || len(rotated.Entries) != config.ServiceLogEntries || rotated.Oldest != 3 {
		t.Fatalf("rotation: reset=%t entries=%d oldest=%d", rotated.Reset, len(rotated.Entries), rotated.Oldest)
	}
	if initial.Entries[0].Text != "msg=first" {
		t.Fatal("rotation mutated an earlier snapshot")
	}
	if !buffer.Snapshot(initial.Session, rotated.Next+1).Reset {
		t.Fatal("future cursor did not reset")
	}
	restarted := New().Snapshot(initial.Session, rotated.Next)
	if !restarted.Reset || restarted.Session == initial.Session || restarted.Next != 0 || restarted.Entries == nil {
		t.Fatalf("restart=%+v", restarted)
	}
}

func TestLargeRecordsHaveBoundedRetentionAndValidUTF8(t *testing.T) {
	buffer := New()
	log := slog.New(slog.NewTextHandler(buffer, nil))
	message := strings.Repeat("🙂", config.ServiceLogEntryBytes)
	writes := config.ServiceLogBytes/config.ServiceLogEntryBytes + 20
	for range writes {
		log.Info(message)
	}
	page := buffer.Snapshot("", 0)
	if len(page.Entries) == 0 || len(page.Entries) >= writes {
		t.Fatal("byte limit did not evict oversized history")
	}
	total := 0
	for _, entry := range page.Entries {
		if !entry.Truncated || !utf8.ValidString(entry.Text) || entry.size() > config.ServiceLogEntryBytes {
			t.Fatalf("invalid truncated entry: bytes=%d valid=%t", entry.size(), utf8.ValidString(entry.Text))
		}
		total += entry.size()
	}
	if total > config.ServiceLogBytes {
		t.Fatalf("retained %d bytes", total)
	}
}

func TestConcurrentWritersAndReaders(t *testing.T) {
	buffer := New()
	log := slog.New(slog.NewTextHandler(buffer, nil))
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			child := log.With("worker", worker)
			for index := range 100 {
				child.Info("message", "index", index)
				page := buffer.Snapshot("", 0)
				if len(page.Entries) == 0 || page.Entries[len(page.Entries)-1].ID != page.Next {
					t.Error("snapshot raced with an append")
				}
			}
		})
	}
	wg.Wait()
	page := buffer.Snapshot("", 0)
	if len(page.Entries) != 800 {
		t.Fatalf("lost records: got %d", len(page.Entries))
	}
	for i, entry := range page.Entries {
		if entry.ID != uint64(i+1) {
			t.Fatal("records are duplicated or out of order")
		}
	}
}
