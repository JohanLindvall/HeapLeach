// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// The file hosts the registry claims by their hosts, read through
// Registry.Extract as a submitted link is: the link reaches its extractor
// without the page, and what the host's API or page says about a removed,
// expired or protected item is the job's error.

// extractorNamed is the extractor of reg named name.
func extractorNamed(t *testing.T, reg *Registry, name string) Extractor {
	t.Helper()
	for _, e := range reg.extractors {
		if e.Name() == name {
			return e
		}
	}
	t.Fatalf("no extractor named %s", name)
	return nil
}

// fileHostSite serves handler, records every path it was asked, and returns
// a registry whose extractor name claims the server's host.
func fileHostSite(t *testing.T, cfg *config.Config, name string, handler http.HandlerFunc) (*Registry, *httptest.Server, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		paths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	if cfg == nil {
		cfg = &config.Config{}
	}
	reg := NewRegistry(cfg, testClient())
	aliasClaims(reg, name, "127.0.0.1")
	return reg, srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// answerJSON answers with status and a JSON body.
func answerJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

// refusePage answers a page request as a site that keeps it from this
// client would: a page the probe could not have read.
func refusePage(w http.ResponseWriter) {
	w.Header().Set(httpx.HeaderContentType, "text/html")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprint(w, `<html><title>Access denied</title></html>`)
}

// keep2ShareClaimed points the registry's services at srv's /api/<service>,
// with the server's host standing for the service named label's domain.
func keep2ShareClaimed(t *testing.T, reg *Registry, srv *httptest.Server, label string) {
	t.Helper()
	k := extractorNamed(t, reg, "keep2share").(*Keep2Share)
	for _, s := range k.services {
		s.api, s.files = srv.URL+"/api/"+s.site(), ""
		if s.label == label {
			s.hosts = append(s.hosts, "127.0.0.1")
		}
	}
}

// A link on FileBoom's domain is FileBoom's, read without its page: its
// API, pace group and label are FileBoom's, whatever Keep2Share's API would
// say about the id, before it is read, after a success and after a failure.
func TestKeep2ShareClaimedLinkTakesItsServiceByDomain(t *testing.T) {
	reg, srv, asked := fileHostSite(t, nil, "keep2share", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch {
		case r.URL.Path == "/api/fileboom/getFileStatus" && in["id"] == "abc123":
			answerJSON(w, http.StatusOK, `{"status":"success","name":"First.bin","size":12,"is_available":true}`)
		case r.URL.Path == "/api/fileboom/getFileStatus":
			answerJSON(w, http.StatusOK, `{"status":"success","is_available":false}`)
		case strings.HasPrefix(r.URL.Path, "/api/"):
			answerJSON(w, http.StatusOK, `{"status":"success","name":"Other.bin","is_available":true}`)
		default:
			refusePage(w)
		}
	})
	keep2ShareClaimed(t, reg, srv, "FileBoom")
	link := srv.URL + "/file/abc123/First.bin"
	if got := reg.Label(mustParse(t, link)); got != "fileboom" {
		t.Errorf("label before reading = %q, want fileboom", got)
	}
	res, ex, err := reg.Extract(t.Context(), link, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Site != "fileboom" || res.Files[0].Name != "First.bin" || *res.Files[0].Pace != fileBoomPace {
		t.Errorf("result = %+v, pace %+v; want FileBoom's file, paced as FileBoom", res, res.Files[0].Pace)
	}
	removed := srv.URL + "/file/gone99"
	if _, ex, err = reg.Extract(t.Context(), removed, Options{}); err == nil ||
		!strings.Contains(err.Error(), "fileboom: file is unavailable or has been removed") {
		t.Errorf("removed file: %v; want FileBoom's own refusal", err)
	} else if got := reg.JobLabel(ex, mustParse(t, removed)); got != "fileboom" {
		t.Errorf("failed job labelled %q, want fileboom", got)
	}
	for _, path := range asked() {
		if !strings.HasPrefix(path, "/api/fileboom/") {
			t.Errorf("asked %s; want FileBoom's API only, and no page", path)
		}
	}
}

// The API's own refusal of an id is the error, with no page asked for and
// so no challenge or probe status standing in for it.
func TestKeep2ShareClaimedLinkReportsTheAPIsError(t *testing.T) {
	reg, srv, asked := fileHostSite(t, nil, "keep2share", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			answerJSON(w, http.StatusBadRequest, `{"status":"error","code":400,"message":"File not found","errorCode":20}`)
			return
		}
		refusePage(w)
	})
	keep2ShareClaimed(t, reg, srv, "Keep2Share")
	_, _, err := reg.Extract(t.Context(), srv.URL+"/file/abc123", Options{})
	if err == nil || !strings.Contains(err.Error(), "keep2share: File not found") {
		t.Errorf("error = %v; want the API's own words", err)
	}
	if paths := asked(); len(paths) != 1 || paths[0] != "/api/keep2share/getFileStatus" {
		t.Errorf("asked %v; want one file status and no page", paths)
	}
}

// A CAPTCHA image is asked for over HTTPS on the service's own domains
// only: another host is asked as the API named it.
func TestKeep2ShareUpgradesCaptchaImagesOnItsOwnDomains(t *testing.T) {
	k := NewKeep2Share(nil)
	for raw, want := range map[string]string{
		"http://k2s.cc/api/v2/captcha?id=1":        "https",
		"http://static.k2s.cc/captcha/1.png":       "https",
		"http://keep2share.cc/captcha/1.png":       "https",
		"https://k2s.cc/captcha/1.png":             "https",
		"http://fboom.me/captcha/1.png":            "http",
		"http://images.example.test/captcha/1.png": "http",
	} {
		u, err := k.services[0].captchaURL(raw)
		if err != nil || u.Scheme != want {
			t.Errorf("%s: %v, %v; want %s", raw, u, err, want)
		}
	}
	if u, err := k.services[1].captchaURL("http://fboom.me/captcha/1.png"); err != nil || u.Scheme != "https" {
		t.Errorf("FileBoom's own image: %v, %v; want https", u, err)
	}
}

// gofileClaimed points the registry's gofile at srv, with a guest account
// already held, and returns it.
func gofileClaimed(t *testing.T, reg *Registry, srv *httptest.Server) *Gofile {
	t.Helper()
	g := extractorNamed(t, reg, "gofile").(*Gofile)
	g.api, g.scriptURL = srv.URL, srv.URL+gofileScriptPath
	g.account.value, g.account.valid, g.account.expires = "guest", true, time.Now().Add(time.Hour)
	return g
}

// A share on gofile's host is read through the content API with no look
// at its page, signed with the secret HEAPLEACH_GOFILE_SECRET configures,
// and gofile's script is not read for one.
func TestGofileClaimedShareSignsWithTheConfiguredSecret(t *testing.T) {
	const secret = "0123456789abcdef"
	var g *Gofile
	reg, srv, asked := fileHostSite(t, &config.Config{GofileSecret: secret, Language: "en-US"}, "gofile",
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/contents/AbCd12" {
				refusePage(w)
				return
			}
			if got, want := r.Header.Get(gofileHeaderWebsiteToken), g.websiteToken("guest", secret, time.Now()); got != want {
				answerJSON(w, http.StatusUnauthorized, `{"status":"error-notPremium"}`)
				return
			}
			answerJSON(w, http.StatusOK, `{"status":"ok","data":{"id":"root","type":"folder","name":"Root",`+
				`"childrenIds":["a"],"children":{"a":{"id":"a","type":"file","name":"a.bin","link":"https://cdn.example.test/a"}}}}`)
		})
	g = gofileClaimed(t, reg, srv)
	res, ex, err := reg.Extract(t.Context(), srv.URL+"/d/AbCd12", Options{})
	if err != nil || ex.Name() != "gofile" || len(res.Files) != 1 {
		t.Fatalf("result %+v from %v, error %v", res, ex, err)
	}
	if paths := asked(); len(paths) != 1 {
		t.Errorf("asked %v; want the content API alone", paths)
	}
}

// A signature gofile refuses points the user at HEAPLEACH_GOFILE_SECRET, and
// content gofile does not hold is reported in its terms.
func TestGofileClaimedShareReportsTheAPIsAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		status     int
		body       string
	}{
		{"refused signature", "HEAPLEACH_GOFILE_SECRET", http.StatusUnauthorized, `{"status":"error-notPremium"}`},
		{"not found", "content not found", http.StatusOK, `{"status":"error-notFound"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, srv, _ := fileHostSite(t, &config.Config{GofileSecret: config.FallbackGofileSecret}, "gofile",
				func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == gofileScriptPath:
						http.NotFound(w, r) // the pinned secret signs
					case strings.HasPrefix(r.URL.Path, "/contents/"):
						answerJSON(w, tc.status, tc.body)
					default:
						refusePage(w)
					}
				})
			gofileClaimed(t, reg, srv)
			if _, _, err := reg.Extract(t.Context(), srv.URL+"/d/AbCd12", Options{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v; want %q", err, tc.want)
			}
		})
	}
}

// Each file host's own refusal of a removed, expired or protected item is
// the job's error, read through the API or the page its extractor reads,
// rather than a probe's status or the generic fallback's.
func TestFileHostClaimsReportTheHostsOwnRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, path, want string
		setup            func(t *testing.T, reg *Registry, srv *httptest.Server)
		handler          http.HandlerFunc
		// page is whether the item's page may be read: the hosts read
		// through an API are never asked for it.
		page bool
	}{
		{
			name: "mediafire", path: "/file/abcdefghij12", page: true,
			want: "the file may have been removed, or need a password",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				fmt.Fprint(w, `<html><head><title>MediaFire</title></head><body><p>Something went wrong.</p></body></html>`)
			},
		},
		{
			name: "filejump", path: "/s/AbCd12", page: true,
			want: "restricted, expired or password protected",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				fmt.Fprint(w, `<html><head><title>A share - FileJump</title></head><body><p>This share has expired.</p></body></html>`)
			},
		},
		{
			name: "pixeldrain", path: "/u/abcd1234", want: "not_found",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/file/abcd1234/info" {
					answerJSON(w, http.StatusNotFound, `{"success":false,"value":"not_found","message":"The entity you requested could not be found"}`)
					return
				}
				refusePage(w)
			},
		},
		{
			name: "filester", path: "/d/AbCd12", want: "filester: sign AbCd12: File not found",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == filesterTokenPath {
					answerJSON(w, http.StatusOK, `{"success":false,"message":"File not found"}`)
					return
				}
				refusePage(w)
			},
		},
		{
			name: "cyberdrop", path: "/f/AbCd12", want: "there is no file AbCd12 (it may have been removed)",
			setup: func(t *testing.T, reg *Registry, srv *httptest.Server) {
				extractorNamed(t, reg, "cyberdrop").(*Share).api = srv.URL
			},
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/file/info/AbCd12" {
					answerJSON(w, http.StatusNotFound, `{"message":"Not found"}`)
					return
				}
				refusePage(w)
			},
		},
		{
			name: "wetransfer", path: "/downloads/abc123/def456", want: "this transfer no longer exists",
			setup: func(t *testing.T, reg *Registry, srv *httptest.Server) {
				extractorNamed(t, reg, "wetransfer").(*Share).api = srv.URL + "/api/v4/transfers"
			},
			handler: func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/") {
					answerJSON(w, http.StatusNotFound, `{"message":"Couldn't find Transfer with the given id"}`)
					return
				}
				refusePage(w)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, srv, asked := fileHostSite(t, nil, tc.name, tc.handler)
			if tc.setup != nil {
				tc.setup(t, reg, srv)
			}
			_, ex, err := reg.Extract(t.Context(), srv.URL+tc.path, Options{})
			if err == nil || !strings.Contains(err.Error(), tc.want) || ex == nil || ex.Name() != tc.name {
				t.Fatalf("%v from %v; want %s's %q", err, ex, tc.name, tc.want)
			}
			if !tc.page {
				for _, path := range asked() {
					if path == tc.path {
						t.Errorf("the item's page was asked for: %v", asked())
					}
				}
			}
		})
	}
}

// An emload link is queued with no request at all: its page is read when
// the transfer runs, on the route the transfer was leased, never at
// discovery on this machine's own address.
func TestEmloadClaimedLinkIsReadOnlyAtTransfer(t *testing.T) {
	reg, srv, asked := fileHostSite(t, nil, "emload", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<html><body><div class="nofile">File not found</div></body></html>`)
	})
	res, ex, err := reg.Extract(t.Context(), srv.URL+"/v2/file/AbCd12/First.bin", Options{})
	if err != nil || ex.Name() != "emload" || len(res.Files) != 1 || res.Files[0].Pace.Group != config.GroupEmload {
		t.Fatalf("result %+v from %v, error %v", res, ex, err)
	}
	if paths := asked(); len(paths) != 0 {
		t.Fatalf("discovery asked %v; want no request", paths)
	}
	if _, err := res.Files[0].Resolve(httpx.WithRoute(t.Context(), "emload-route", testClient(), nil)); err == nil ||
		!strings.Contains(err.Error(), "no longer available") {
		t.Errorf("resolve: %v; want the page read and its notice reported", err)
	}
	if paths := asked(); len(paths) != 1 {
		t.Errorf("transfer asked %v; want the page once", paths)
	}
}

// Drive's direct download forms — /uc?id= and the usercontent /download?id=
// — are Drive's: named after the file id, and confirmed again on every
// attempt, rather than a fallback download named "uc".
func TestDriveClaimsItsDirectDownloadLinks(t *testing.T) {
	reg, srv, asked := fileHostSite(t, nil, "drive", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "application/octet-stream")
		fmt.Fprint(w, "bytes")
	})
	extractorNamed(t, reg, "drive").(*GoogleDrive).download = srv.URL + "/confirm"
	for _, path := range []string{"/uc?id=AbCdEfGhIjKl&export=download", "/download?id=AbCdEfGhIjKl&export=download"} {
		before := len(asked())
		res, ex, err := reg.Extract(t.Context(), srv.URL+path, Options{})
		if err != nil || ex.Name() != "drive" || res.Title != "drive-AbCdEfGhIjKl" || res.Files[0].Resolve == nil {
			t.Fatalf("%s: result %+v from %v, error %v", path, res, ex, err)
		}
		if _, err := res.Files[0].Resolve(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, p := range asked()[before:] {
			if p != "/confirm" {
				t.Errorf("%s: asked %s; want only the confirmation endpoint", path, p)
			}
		}
		if n := len(asked()) - before; n != 2 {
			t.Errorf("%s: confirmed %d times; want once at discovery and once per attempt", path, n)
		}
	}
}

// Dropbox's dl=1 and content-host links are Dropbox's, named after the
// file; a link on the site is rotated across the site and its content host.
func TestDropboxClaimsItsDirectDownloadLinks(t *testing.T) {
	reg := testRegistry(t, nil)
	res, ex, err := reg.Extract(t.Context(), "https://www.dropbox.com/s/abc123/First.zip?dl=1", Options{})
	if err != nil || ex.Name() != "dropbox" || res.Title != "First.zip" || res.Files[0].Resolve == nil {
		t.Fatalf("result %+v from %v, error %v", res, ex, err)
	}
	hosts := map[string]bool{}
	for range 2 {
		target, err := res.Files[0].Resolve(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(target.URL)
		hosts[u.Hostname()] = true
	}
	if !hosts["www.dropbox.com"] || !hosts[dropboxContentHost] {
		t.Errorf("attempts went to %v; want the site and its content host", hosts)
	}
	content := "https://dl.dropboxusercontent.com/s/abc123/First.zip"
	res, ex, err = reg.Extract(t.Context(), content, Options{})
	if err != nil || ex.Name() != "dropbox" || res.Title != "First.zip" || !strings.HasPrefix(res.Files[0].URL, content+"?") {
		t.Fatalf("content host: result %+v from %v, error %v", res, ex, err)
	}
}

// A link to ImagePond's media host is ImagePond's file, asked for with the
// site's Referer, and labelled as ImagePond's job.
func TestImagePondClaimsItsMediaHost(t *testing.T) {
	reg := testRegistry(t, nil)
	link := "https://media.imagepond.net/media/images/First.jpg"
	if got := reg.Label(mustParse(t, link)); got != "imagepond" {
		t.Errorf("label = %q, want imagepond", got)
	}
	res, ex, err := reg.Extract(context.Background(), link, Options{})
	if err != nil || ex.Name() != "imagepond" || len(res.Files) != 1 {
		t.Fatalf("result %+v from %v, error %v", res, ex, err)
	}
	if f := res.Files[0]; f.URL != link || f.Name != "First.jpg" || f.Headers[httpx.HeaderReferer] != imagePondRoot+"/" {
		t.Errorf("file = %+v; want the link, with the site's Referer", f)
	}
}
