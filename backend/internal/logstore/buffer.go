// SPDX-License-Identifier: MIT

// Package logstore retains a bounded window of the service's formatted logs.
package logstore

import (
	"crypto/rand"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Entry is one TextHandler record, with its standard header split out for
// filtering and display. Text keeps the message and all inherited fields.
type Entry struct {
	ID        uint64 `json:"id"`
	Time      string `json:"time"`
	Level     string `json:"level"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Page carries only entries after a cursor, unless a restart or retention
// gap requires the browser to replace its previous window.
type Page struct {
	Session string  `json:"session"`
	Entries []Entry `json:"entries"`
	Oldest  uint64  `json:"oldest"`
	Next    uint64  `json:"next"`
	Limit   int     `json:"limit"`
	Reset   bool    `json:"reset"`
}

// Buffer is a concurrent, in-memory window shared by the logger and API.
type Buffer struct {
	mu      sync.Mutex
	session string
	entries []Entry
	head    int
	count   int
	bytes   int
	next    uint64
}

func New() *Buffer {
	return &Buffer{session: rand.Text(), entries: make([]Entry, config.ServiceLogEntries)}
}

// Write accepts the single, complete write made by slog.TextHandler for
// each record. Its quoting keeps newlines inside messages in that record.
// It never waits on a browser or changes the normal console output.
func (b *Buffer) Write(p []byte) (int, error) {
	end := len(p)
	if end > 0 && p[end-1] == '\n' {
		end--
	}
	truncated := end > config.ServiceLogEntryBytes
	if truncated {
		end = config.ServiceLogEntryBytes
		for end > 0 && !utf8.RuneStart(p[end]) {
			end--
		}
	}
	entry := Entry{Level: "INFO", Text: string(p[:end]), Truncated: truncated}
	// These are TextHandler's fixed leading fields, before any user fields
	// or groups. A record with a zero timestamp omits only the time field.
	if first, rest, ok := strings.Cut(entry.Text, " "); ok && strings.HasPrefix(first, "time=") {
		entry.Time, entry.Text = strings.TrimPrefix(first, "time="), rest
	}
	if first, rest, ok := strings.Cut(entry.Text, " "); ok && strings.HasPrefix(first, "level=") {
		entry.Level, entry.Text = strings.TrimPrefix(first, "level="), rest
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	entry.ID = b.next
	for b.count > 0 && (b.count == len(b.entries) || b.bytes+entry.size() > config.ServiceLogBytes) {
		b.bytes -= b.entries[b.head].size()
		b.entries[b.head] = Entry{}
		b.head = (b.head + 1) % len(b.entries)
		b.count--
	}
	b.entries[(b.head+b.count)%len(b.entries)] = entry
	b.count++
	b.bytes += entry.size()
	return len(p), nil
}

func (e Entry) size() int { return len(e.Time) + len(e.Level) + len(e.Text) }

// Snapshot copies immutable entries under the lock; encoding and slow
// clients happen afterwards and cannot hold up logging or download workers.
func (b *Buffer) Snapshot(session string, after uint64) Page {
	b.mu.Lock()
	defer b.mu.Unlock()
	oldest := b.next + 1
	if b.count > 0 {
		oldest = b.entries[b.head].ID
	}
	reset := session != b.session || after > b.next || after < oldest-1
	page := Page{Session: b.session, Entries: []Entry{}, Oldest: oldest, Next: b.next,
		Limit: len(b.entries), Reset: reset}
	for i := range b.count {
		entry := b.entries[(b.head+i)%len(b.entries)]
		if reset || entry.ID > after {
			page.Entries = append(page.Entries, entry)
		}
	}
	return page
}
