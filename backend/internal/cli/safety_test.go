package cli

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/JohanLindvall/HeapLeach/internal/download"
)

func TestRemoteTextCannotControlTheTerminal(t *testing.T) {
	const remote = "Ångström\x1b[2J\x1b]52;c;payload\a\r\n\t\u009b2J\u2028終"
	r := newRenderer(io.Discard, 200, false, false)
	it := download.ItemView{Name: remote, Error: remote, Note: remote, Status: download.StatusRunning}
	lines := []string{
		r.jobLine(download.JobView{Title: remote, Host: remote, Total: 1}),
		r.jobLine(download.JobView{Source: remote, Error: remote, Status: download.StatusFailed}),
		r.itemRow(it), firstLine(remote),
	}
	for _, status := range []download.Status{download.StatusDone, download.StatusFailed, download.StatusCanceled} {
		done := it
		done.Status = status
		lines = append(lines, r.itemDoneLine(done))
	}
	lines = append(lines, frame(download.Snapshot{Jobs: []download.JobView{{Items: []download.ItemView{it}}}}, r, time.Now())...)
	for _, line := range lines {
		if strings.ContainsFunc(line, func(r rune) bool { return unicode.IsControl(r) || r == '\u2028' || r == '\u2029' }) {
			t.Errorf("remote terminal control survived: %q", line)
		}
	}
	if got := terminalText("Ångström 終"); got != "Ångström 終" {
		t.Errorf("ordinary Unicode changed: %q", got)
	}
}

func TestRunFailsWhenSourceResolutionFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><title>Empty listing</title></html>")
	}))
	defer srv.Close()
	var out bytes.Buffer
	err := Run(t.Context(), newRunManager(t), Options{URLs: []string{"links:" + srv.URL}, Out: &out, Width: 100})
	if !errors.Is(err, ErrIncomplete) || !strings.Contains(out.String(), "sources failed to resolve") {
		t.Fatalf("Run = %v, output = %s", err, out.String())
	}
}

func TestRunFailsWhenOnlySomeInputsAreAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "hello")
	}))
	defer srv.Close()
	var out bytes.Buffer
	err := Run(t.Context(), newRunManager(t), Options{
		URLs: []string{"::invalid::", srv.URL + "/sample.bin"}, Out: &out, Width: 100,
	})
	if !errors.Is(err, ErrIncomplete) || !strings.Contains(out.String(), "1 file,") {
		t.Fatalf("Run = %v, output = %s", err, out.String())
	}
}

func TestSummaryReportsCanceledResolution(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, 80, false, false)
	snap := download.Snapshot{Jobs: []download.JobView{{Status: download.StatusCanceled}}}
	if err := summary(snap, &out, r, time.Now()); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("summary = %v", err)
	}
}
