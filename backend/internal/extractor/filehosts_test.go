// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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
			res := extractOK(t, NewDropbox(nil), tc.raw, Options{})
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

func TestDropboxRecognise(t *testing.T) {
	d := NewDropbox(nil)
	viewer := `<html><head><title>Dropbox</title><meta property="og:site_name" content="Dropbox">` +
		`<script src="/static/atlas/file_viewer/page_file-vflA1.js"></script></head><body></body></html>`
	link := "https://share.example.test/scl/fi/abc123/holiday.zip?rlkey=xyz&dl=0"
	expectRecognised(t, d, htmlPage(t, link, viewer), true)
	expectRecognised(t, d, htmlPage(t, "https://share.example.test/s/abc123/holiday.zip", viewer), true)
	expectRecognised(t, d, htmlPage(t, link, `<html><head><title>Dropbox</title></head></html>`), false)
	expectRecognised(t, d, htmlPage(t, "https://share.example.test/plans", viewer), false)
	expectShape(t, d, link, true)
	expectShape(t, d, "https://share.example.test/scl/fo/abc123/xyz?rlkey=k", true)
	expectShape(t, d, "https://share.example.test/s/abc123", false)
	expectShape(t, d, "https://share.example.test/", false)
}

func TestMediafireRecognise(t *testing.T) {
	m := NewMediafire(nil)
	file := `<html><head><title>First Archive.zip - MediaFire</title><meta property="og:site_name" content="MediaFire"></head>` +
		`<body><a id="downloadButton" href="https://download.example.test/key/First+Archive.zip">Download</a></body></html>`
	folder := `<html><head><title>My Files</title><meta property="og:site_name" content="MediaFire"></head>` +
		`<body><script type="text/template"><li class="row_container mf_filecontainer file"></li></script></body></html>`
	expectRecognised(t, m, htmlPage(t, "https://share.example.test/file/abc123def45/First_Archive.zip/file", file), true)
	expectRecognised(t, m, htmlPage(t, "https://share.example.test/folder/abc123def45/Example", folder), true)
	// The form and the name, without the button or the folder viewer.
	named := `<html><head><meta property="og:site_name" content="MediaFire"></head><body></body></html>`
	expectRecognised(t, m, htmlPage(t, "https://share.example.test/file/abc123def45/First_Archive.zip/file", named), false)
	expectRecognised(t, m, htmlPage(t, "https://share.example.test/folder/abc123def45", named), false)
	// The button elsewhere, or under another name.
	expectRecognised(t, m, htmlPage(t, "https://share.example.test/about", file), false)
	expectRecognised(t, m, htmlPage(t, "https://share.example.test/file/abc123def45",
		`<html><title>A blog</title><a id="downloadButton" href="/x.zip">Download</a></html>`), false)
	expectShape(t, m, "https://share.example.test/file/abc123def45/First_Archive.zip/file", true)
	expectShape(t, m, "https://share.example.test/?abc123def4567", true)
	expectShape(t, m, "https://share.example.test/folder/abc123def45", true)
	expectShape(t, m, "https://share.example.test/?utm_source=x", false)
	expectShape(t, m, "https://share.example.test/upgrade", false)
}

func TestGoogleDriveRecognise(t *testing.T) {
	g := NewGoogleDrive(nil)
	viewer := `<html><head><title>First Report.pdf - Google Drive</title>` +
		`<script src="/_/apps-fileview/_/js/k=apps-fileview.v.1/m=v"></script></head><body></body></html>`
	confirm := `<html><body><form id="download-form" action="https://download.example.test/download">` +
		`<input type="hidden" name="id" value="FILEID123456"><input type="hidden" name="uuid" value="u"></form></body></html>`
	expectRecognised(t, g, htmlPage(t, "https://share.example.test/file/d/FILEID123456/view?usp=sharing", viewer), true)
	expectRecognised(t, g, htmlPage(t, "https://share.example.test/uc?export=download&id=FILEID123456", confirm), true)
	// The form and the name, without the viewer's bundle.
	expectRecognised(t, g, htmlPage(t, "https://share.example.test/file/d/FILEID123456/view",
		`<html><head><title>First Report.pdf - Google Drive</title></head></html>`), false)
	expectRecognised(t, g, htmlPage(t, "https://share.example.test/drive/folders/FOLDERID123", viewer), false)
	expectShape(t, g, "https://share.example.test/file/d/FILEID123456/view", true)
	expectShape(t, g, "https://share.example.test/open?id=FILEID123456", true)
	expectShape(t, g, "https://share.example.test/uc?export=download&id=FILEID123456", true)
	expectShape(t, g, "https://share.example.test/uc", false)
	expectShape(t, g, "https://share.example.test/drive/folders/FOLDERID123", false)
}

// mediafireFolderServer serves folder listings by "<key> <kind> <chunk>",
// each the JSON of one chunk; anything else fails.
func mediafireFolderServer(t *testing.T, chunks map[string]string) (*Share, *atomic.Int32) {
	t.Helper()
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/folder/get_content.php" {
			http.NotFound(w, r)
			return
		}
		asked.Add(1)
		body, ok := chunks[q.Get("folder_key")+" "+q.Get("content_type")+" "+q.Get("chunk")]
		if !ok {
			http.Error(w, "unavailable", http.StatusForbidden)
			return
		}
		fmt.Fprintf(w, `{"response":{"result":"Success","folder_content":%s}}`, body)
	}))
	t.Cleanup(srv.Close)
	share := newShare(testClient(), &mediafireSite)
	share.api = srv.URL
	return share, &asked
}

// TestMediafireFolderPages covers a folder share's chunks: a repeated chunk
// ends the files with a note, a subfolder whose own subfolders will not list
// keeps its files and says so, and the file limit stops the reading.
func TestMediafireFolderPages(t *testing.T) {
	m, _ := mediafireFolderServer(t, map[string]string{
		"root files 1":    `{"files":[{"quickkey":"qa","filename":"a.bin"}],"more_chunks":"yes"}`,
		"root files 2":    `{"files":[{"quickkey":"qa","filename":"a.bin"}],"more_chunks":"yes"}`,
		"root folders 1":  `{"folders":[{"folderkey":"sub","name":"Sub / Folder"}],"more_chunks":"no"}`,
		"sub files 1":     `{"files":[{"quickkey":"qb","filename":"b.bin"}],"more_chunks":"no"}`,
		"nothing files 1": `{"files":[],"more_chunks":"no"}`,
	})
	res, err := mediafireShare(context.Background(), m, "root", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(filePaths(res.Files), ","); got != "a.bin,Sub _ Folder/b.bin" {
		t.Errorf("files = %s", got)
	}
	if res.Note != "partial — repeated or empty page; unreadable folders" {
		t.Errorf("note = %q", res.Note)
	}

	// An empty last chunk is the end of the listing, nothing to note.
	leaf, _ := mediafireFolderServer(t, map[string]string{
		"leaf files 1":   `{"files":[{"quickkey":"qa","filename":"a.bin"}],"more_chunks":"no"}`,
		"leaf folders 1": `{"folders":[],"more_chunks":"no"}`,
	})
	if res, err := mediafireShare(context.Background(), leaf, "leaf", Options{}); err != nil || len(res.Files) != 1 || res.Note != "" {
		t.Errorf("a folder of one file: %+v, %v", res, err)
	}

	// A folder whose subfolders will not list and that holds no files of
	// its own is a folder that could not be read.
	if _, err := mediafireShare(context.Background(), m, "nothing", Options{}); err == nil {
		t.Error("an unreadable folder resolved")
	}

	full, asked := mediafireFolderServer(t, map[string]string{
		"root files 1": `{"files":[{"quickkey":"qa","filename":"a.bin"},{"quickkey":"qb","filename":"b.bin"}],"more_chunks":"yes"}`,
	})
	res, err = mediafireShare(context.Background(), full, "root", Options{limits: Limits{Files: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Note != "partial — file limit" || asked.Load() != 1 {
		t.Errorf("files = %d, note = %q, asked %d; want 1, the file limit, one chunk", len(res.Files), res.Note, asked.Load())
	}
}
