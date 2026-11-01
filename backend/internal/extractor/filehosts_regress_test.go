// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Drive words its quota and access refusals as a page, which carries
// neither the viewer nor the confirmation form: such a page of a Drive file
// form is still Drive's, and its refusal is the answer.
func TestDriveRecognisesItsRefusalPages(t *testing.T) {
	g := NewGoogleDrive(nil)
	quota := `<html><title>Google Drive - Quota exceeded</title><p>Too many users have viewed or downloaded this file recently.</p></html>`
	access := `<html><title>Google Drive</title><p>You need access</p><button>Request access</button></html>`
	for _, link := range []string{
		"https://files.example.test/uc?id=AAAAAAAAAAAA&export=download",
		"https://files.example.test/download?id=AAAAAAAAAAAA",
		"https://files.example.test/u/0/uc?id=AAAAAAAAAAAA&export=download",
	} {
		expectRecognised(t, g, htmlPage(t, link, quota), true)
		expectRecognised(t, g, fixturePage(t, link, http.StatusForbidden, "text/html", access), true)
		expectRecognised(t, g, htmlPage(t, link, `<p>an ordinary page</p>`), false)
	}
	p := htmlPage(t, "https://files.example.test/uc?id=AAAAAAAAAAAA&export=download", quota)
	_, err := g.Extract(withPage(context.Background(), p), p.URL, Options{})
	if err == nil || !strings.Contains(err.Error(), "download quota") {
		t.Errorf("err = %v, want Drive's quota refusal", err)
	}
}

// A list or directory id of any length is asked of the origin's API when the
// probe was withheld; a served page without the viewer keeps the
// eight-character bound.
func TestPixeldrainAsksAboutAnyIDOfAWithheldPage(t *testing.T) {
	srv, _ := jsonServer(t, map[string]string{
		"/api/list/SampleList": `{"success":true,"title":"Samples","files":[{"id":"SampleID","name":"sample.bin","size":12,"mime_type":"application/octet-stream"}]}`,
		"/api/filesystem/Shared": `{"base_index":0,"path":[{"type":"dir","name":"Shared"}],` +
			`"children":[{"type":"file","name":"sample.bin","file_size":12}]}`,
	})
	px := NewPixeldrain(testClient())
	for _, path := range []string{"/l/SampleList", "/d/Shared"} {
		if !px.Recognise(context.Background(), refusedProbe(t, srv.URL+path)) {
			t.Errorf("%s: a withheld page whose API answers was not taken", path)
		}
		if px.Recognise(context.Background(), htmlPage(t, srv.URL+path, "<p>a landing page</p>")) {
			t.Errorf("%s: a served page of another length was asked about", path)
		}
	}
}

// A share's download route redirects to signed storage, so its probe ends
// off the share's forms; it is read by the share page it belongs to, which
// gives the job its name and size, and the stable route is kept.
func TestFileJumpReadsARedirectedDownloadRouteByItsShare(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/s/test-share/download", "/s/test-share/content":
			http.Redirect(w, r, "/storage/signed.bin", http.StatusFound)
		case "/storage/signed.bin":
			w.Header().Set(httpx.HeaderContentType, "application/octet-stream")
			_, _ = w.Write([]byte("bytes"))
		case "/s/test-share":
			w.Header().Set(httpx.HeaderContentType, "text/html")
			_, _ = w.Write([]byte(`<title>Shared file</title><div class="title">First Song.mp3</div>` +
				`<div class="meta">Size: 1.5 MB</div><a class="btn" href="/s/test-share/download">Download</a>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := testClient()
	reg := &Registry{client: client, fallback: NewDirect(client)}
	reg.extractors = []Extractor{NewFileJump(client)}
	for _, route := range []string{"/download", "/content"} {
		res, ex, err := reg.Extract(context.Background(), srv.URL+"/s/test-share"+route, Options{})
		if err != nil || ex.Name() != "filejump" {
			t.Fatalf("%s: extractor %v, err %v", route, ex, err)
		}
		if res.Title != "First Song.mp3" || res.Files[0].URL != srv.URL+"/s/test-share/download" {
			t.Errorf("%s: title %q, file %q", route, res.Title, res.Files[0].URL)
		}
	}
}

// A file page that names its file only through the download button's
// dl-server link is recognised by that link's id, as its extraction reads it.
func TestBunkrRecognisesAFileByItsDownloadLink(t *testing.T) {
	b := NewBunkr(nil)
	expectRecognised(t, b, htmlPage(t, "https://files.example.test/f/AAAA",
		`<html><title>First Clip.mp4 | A Host</title><a class="btn" href="https://dl.files.example.test/file/12345">Download</a></html>`), true)
	// A link to some other download server's path is not the button.
	expectRecognised(t, b, htmlPage(t, "https://files.example.test/f/AAAA",
		`<html><title>A blog</title><a href="https://cdn.example.test/file/12345">x</a></html>`), false)
}

// A file page whose final URL ends in a slash is the same page as without.
func TestSuvoboxFilePageWithATrailingSlash(t *testing.T) {
	s := NewSuvobox(nil)
	page := `<html><a class="btn" href="/f/aaaa1111?dl=1">Download</a></html>`
	expectRecognised(t, s, htmlPage(t, "https://files.example.test/f/aaaa1111/", page), true)
	expectRecognised(t, s, htmlPage(t, "https://files.example.test/f/aaaa1111", page), true)
	expectRecognised(t, s, htmlPage(t, "https://files.example.test/f/bbbb2222/", page), false)
}

// A file link on a service's own domains folds with the same id on that
// service's other domains, never with the other service's: the two keep
// their file stores apart. The same form on any other host is that host's
// link, which can neither shadow nor be shadowed by the services'.
func TestKeep2ShareFileKeyKeepsTheServicesApart(t *testing.T) {
	reg := NewRegistry(&config.Config{}, nil)
	// Another host the link scanner follows as the service's own.
	aliasHosts(reg, "keep2share", "one.example.test", "two.example.test")
	links := []string{
		"https://one.example.test/file/abc123",
		"https://two.example.test/file/abc123/clip.mp4",
		"https://one.example.test/file/abc123/again.mp4?ref=x",
		"https://k2s.cc/file/abc123/clip.mp4",
		"https://keep2share.cc/file/abc123",
		"https://www.k2s.cc/file/abc123?site=x",
		"https://fboom.me/file/abc123",
		"https://fboom.me/file/abc123/clip.mp4",
	}
	from, _ := ParseURL("https://forum.example.test/thread")
	got := fileLinkSources(reg, links, from)
	want := []string{links[0], links[1], links[3], links[6]}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("sources = %v, want %v", got, want)
	}
}

// A job read from the second service is labelled by that service, and the
// label is among the registry's hosts, as the UI counts and filters by them.
func TestKeep2ShareLabelsAJobByItsService(t *testing.T) {
	for _, label := range []string{"Keep2Share", "FileBoom"} {
		k := keep2ShareProtocolTestSite(t, label, func(http.ResponseWriter, map[string]string) {})
		u := mustParse(t, "https://share.example.test/file/test-file")
		res, err := k.ex.Extract(keep2SharePageContext(t, u, label), u, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.ToLower(label); res.Site != want {
			t.Errorf("%s: site = %q, want %q", label, res.Site, want)
		}
	}
	hosts := NewRegistry(&config.Config{}, nil).Hosts()
	if !slices.Contains(hosts, "fileboom") || !slices.Contains(hosts, "keep2share") {
		t.Errorf("hosts %v lack a service label", hosts)
	}
}

// A folder link and an account-prefixed endpoint keep Drive's handling: the
// endpoint is a file form, and a folder is refused in Drive's terms rather
// than called a web page nobody recognises.
func TestDriveFolderAndAccountForms(t *testing.T) {
	g := NewGoogleDrive(nil)
	expectShape(t, g, "https://files.example.test/u/0/uc?id=AAAAAAAAAAAA&export=download", true)
	expectShape(t, g, "https://files.example.test/drive/folders/AAAAAAAAAAAA", true)
	expectShape(t, g, "https://files.example.test/drive/u/1/folders/AAAAAAAAAAAA", true)
	expectShape(t, g, "https://files.example.test/drive/my-drive", false)

	folder := htmlPage(t, "https://files.example.test/drive/folders/AAAAAAAAAAAA",
		`<html><head><title>A folder - Google Drive</title></head></html>`)
	expectRecognised(t, g, folder, true)
	expectRecognised(t, g, htmlPage(t, folder.URL.String(), `<html><title>A blog</title></html>`), false)
	_, err := g.Extract(withPage(context.Background(), folder), folder.URL, Options{})
	if err == nil || !strings.Contains(err.Error(), "folder links are not supported") {
		t.Errorf("err = %v, want the folder refusal", err)
	}
}
