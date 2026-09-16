package download

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResumeRestartsWhenTheResourceLengthChanges(t *testing.T) {
	payload := []byte("new123")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=3-8" {
			w.Header().Set("Content-Range", "bytes 3-5/6")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[3:])
			return
		}
		w.Header().Set("Content-Range", "bytes 0-5/6")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	m := progressManager(t, 1)
	it := &Item{ID: newID(), URL: srv.URL, Name: "file.bin", Size: -1}
	part := filepath.Join(m.DownloadDir(), "file.bin."+partSuffix(it.URL)+".part")
	if err := os.WriteFile(part, []byte("old______"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &transferState{Size: 9, Segments: []segmentState{{Start: 0, Pos: 3, End: 9}}}
	if err := saveTransferState(part, st); err != nil {
		t.Fatal(err)
	}
	if err := m.transfer(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(m.DownloadDir(), "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("finished file = %q, want %q", got, payload)
	}
}

func TestResumeRejectsUnprovenCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, contentRange  string
		segmented, complete bool
	}{
		{name: "no length"},
		{name: "shorter resource", contentRange: "bytes */3"},
		{name: "longer resource", contentRange: "bytes */9"},
		{name: "invalid unit", contentRange: "items */6"},
		{name: "satisfied range", contentRange: "bytes 0-5/6"},
		{name: "segmented part", contentRange: "bytes */2", segmented: true},
		{name: "complete sequential part", contentRange: "bytes */6", complete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", tc.contentRange)
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			}))
			defer srv.Close()
			m, it := testTransferDeps(t, srv.URL)
			part := filepath.Join(t.TempDir(), "file.part")
			if err := os.WriteFile(part, []byte("abcdef"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.segmented {
				st := &transferState{Size: 9, Segments: []segmentState{{Start: 0, Pos: 2, End: 3}, {Start: 3, Pos: 6, End: 9}}}
				if err := saveTransferState(part, st); err != nil {
					t.Fatal(err)
				}
			}
			_, err := m.transferOnce(context.Background(), it, part, "file.bin")
			if (err == nil) != tc.complete {
				t.Fatalf("completion = %v, want %v (err: %v)", err == nil, tc.complete, err)
			}
			if tc.complete && it.downloaded.Load() != 6 {
				t.Errorf("credited %d bytes, want 6", it.downloaded.Load())
			}
		})
	}
}

func TestResumeRestartsWhenTheCheckpointCannotAccountForThePart(t *testing.T) {
	for _, mode := range []string{"missing part", "truncated part", "corrupt checkpoint"} {
		t.Run(mode, func(t *testing.T) {
			payload := []byte("abcdef")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Range"); got != "bytes=0-" {
					t.Errorf("resumed unproven bytes with %q", got)
				}
				w.Header().Set("Content-Range", "bytes 0-5/6")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(payload)
			}))
			defer srv.Close()
			m, it := testTransferDeps(t, srv.URL)
			part := filepath.Join(t.TempDir(), "file.part")
			if mode != "missing part" {
				if err := os.WriteFile(part, []byte("old"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			st := &transferState{Size: 6, Segments: []segmentState{{Start: 0, Pos: 6, End: 6}}}
			if err := saveTransferState(part, st); err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt checkpoint" {
				if err := os.WriteFile(statePath(part), []byte("{"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.transferOnce(context.Background(), it, part, "file.bin"); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(part)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("part = %q, want %q", got, payload)
			}
		})
	}
}

func TestPartialResponsesAreValidatedBeforeWriting(t *testing.T) {
	for _, tc := range []struct{ name, contentRange, body string }{
		{"missing header", "", "def"},
		{"wrong offset", "bytes 0-2/6", "abc"},
		{"wrong length", "bytes 3-5/6", "de"},
		{"invalid total", "bytes 3-5/5", "def"},
		{"invalid unit", "items 3-5/6", "def"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", tc.contentRange)
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			m, it := testTransferDeps(t, srv.URL)
			part := filepath.Join(t.TempDir(), "file.part")
			if err := os.WriteFile(part, []byte("abc"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := m.transferOnce(context.Background(), it, part, "file.bin"); err == nil {
				t.Error("primary accepted an invalid partial response")
			}
			got, err := os.ReadFile(part)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "abc" {
				t.Errorf("invalid response changed the part to %q", got)
			}
			tr := &segmentedTransfer{manager: m, item: it, table: newSegmentTable(6, 3)}
			body, err := tr.open(context.Background(), newSegment(3, 3, 6), true)
			if err == nil {
				body.Close()
				t.Error("extra connection accepted an invalid partial response")
			}
		})
	}
}
