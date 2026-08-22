package extractor

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

func TestDropboxAsksForTheBytes(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantName string
		wantPath string
	}{
		{
			name:     "share link",
			raw:      "https://www.dropbox.com/s/abc123/holiday.zip?dl=0",
			wantName: "holiday.zip",
			wantPath: "/s/abc123/holiday.zip",
		},
		{
			name:     "current share link",
			raw:      "https://www.dropbox.com/scl/fi/abc123/holiday.zip?rlkey=xyz&dl=0",
			wantName: "holiday.zip",
			wantPath: "/scl/fi/abc123/holiday.zip",
		},
		{
			name: "folder link",
			raw:  "https://www.dropbox.com/scl/fo/abc123/xyz?rlkey=k",
			// No extension, so the server's own Content-Disposition wins.
			wantName: "dropbox-folder",
			wantPath: "/scl/fo/abc123/xyz",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := ParseURL(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			res, err := NewDropbox(nil).Extract(context.Background(), u, Options{})
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			if len(res.Files) != 1 {
				t.Fatalf("got %d files, want 1", len(res.Files))
			}
			file := res.Files[0]
			if file.Name != tc.wantName {
				t.Errorf("name = %q, want %q", file.Name, tc.wantName)
			}

			direct, err := url.Parse(file.URL)
			if err != nil {
				t.Fatal(err)
			}
			if direct.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", direct.Path, tc.wantPath)
			}
			if got := direct.Query().Get("dl"); got != "1" {
				t.Errorf("dl = %q, want 1 — without it dropbox serves the viewer", got)
			}

			// The content host is offered as a second source.
			if file.Resolve == nil {
				t.Fatal("no mirror to fail over to")
			}
			first, err := file.Resolve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			second, err := file.Resolve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if first.URL == second.URL {
				t.Error("both attempts resolved to the same host")
			}
			if !strings.Contains(second.URL, dropboxContentHost) {
				t.Errorf("the second attempt is %q, want it on %s", second.URL, dropboxContentHost)
			}
		})
	}
}

func TestDriveFileID(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "https://drive.google.com/file/d/FILEID123/view?usp=sharing", want: "FILEID123"},
		{raw: "https://drive.google.com/open?id=FILEID123", want: "FILEID123"},
		{raw: "https://drive.google.com/uc?export=download&id=FILEID123", want: "FILEID123"},
		{raw: "https://docs.google.com/uc?id=FILEID123", want: "FILEID123"},
		{raw: "https://drive.google.com/drive/folders/FOLDERID", want: ""},
		{raw: "https://drive.google.com/", want: ""},
	}
	for _, tc := range tests {
		u, err := ParseURL(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := driveFileID(u); got != tc.want {
			t.Errorf("driveFileID(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestDriveRejectsFolders keeps the refusal explicit: a folder link parses
// perfectly well and simply has no file behind it, so the message has to say
// why rather than reporting nothing found.
func TestDriveRejectsFolders(t *testing.T) {
	u, err := ParseURL("https://drive.google.com/drive/folders/FOLDERID")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewGoogleDrive(nil).Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("a folder link was accepted")
	}
	if !strings.Contains(err.Error(), "folder") {
		t.Errorf("error %q does not explain that folders are unsupported", err)
	}
}

func TestDriveRefusalMessages(t *testing.T) {
	quota := driveRefusal("<p>Too many users have viewed or downloaded this file recently.</p>")
	if !strings.Contains(quota, "quota") {
		t.Errorf("the quota page reads as %q", quota)
	}
	if driveRefusal("<html><body>ordinary page</body></html>") != "" {
		t.Error("an ordinary page was read as a refusal")
	}
}

func TestFileHostMatching(t *testing.T) {
	tests := []struct {
		host  string
		match func(*url.URL) bool
		want  bool
	}{
		{host: "www.dropbox.com", match: NewDropbox(nil).Match, want: true},
		{host: "dl.dropboxusercontent.com", match: NewDropbox(nil).Match, want: true},
		{host: "dropbox.example.test", match: NewDropbox(nil).Match, want: false},
		{host: "www.mediafire.com", match: NewMediafire(nil).Match, want: true},
		{host: "mediafire.com", match: NewMediafire(nil).Match, want: true},
		{host: "drive.google.com", match: NewGoogleDrive(nil).Match, want: true},
		{host: "drive.usercontent.google.com", match: NewGoogleDrive(nil).Match, want: true},
		{host: "docs.google.com", match: NewGoogleDrive(nil).Match, want: false},
	}
	for _, tc := range tests {
		u := &url.URL{Scheme: "https", Host: tc.host, Path: "/"}
		if got := tc.match(u); got != tc.want {
			t.Errorf("%s matched = %v, want %v", tc.host, got, tc.want)
		}
	}
}
