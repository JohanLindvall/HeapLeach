// SPDX-License-Identifier: MIT

package extractor

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// aliasClaims makes hosts stand in for name's own in what reg claims by
// host (hostClaims): a fixture's local server playing the site.
func aliasClaims(reg *Registry, name string, hosts ...string) {
	if reg.claimAliases == nil {
		reg.claimAliases = map[string][]string{}
	}
	reg.claimAliases[name] = append(reg.claimAliases[name], hosts...)
}

// TestHostClaimedFirstIsNotProbed: an extractor that reads an API or hands
// the link to yt-dlp takes a link on its hosts before any probe; the page
// is never asked for, and no page is attached.
func TestHostClaimedFirstIsNotProbed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "refused", http.StatusForbidden)
	}))
	defer srv.Close()
	reg := NewRegistry(&config.Config{}, testClient())
	aliasClaims(reg, "gofile", "127.0.0.1")
	ex, page := reg.identify(t.Context(), mustParse(t, srv.URL+"/d/AbCd12"))
	if ex.Name() != "gofile" || page != nil || hits.Load() != 0 {
		t.Errorf("identify = %s, page %v after %d requests; want gofile, no page, no request", ex.Name(), page != nil, hits.Load())
	}
	if got := reg.Label(mustParse(t, srv.URL+"/anything")); got != "gofile" {
		t.Errorf("label %q, want gofile", got)
	}
}

// TestHostClaimedFirstByFormLeavesOtherPages: a broadcaster takes its
// programme links first, by their form on its hosts; the host's other
// pages (news, about) are recognised like any page, and left to the generic
// fallback when nothing recognises them.
func TestHostClaimedFirstByFormLeavesOtherPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<html><title>News</title></html>`)
	}))
	defer srv.Close()
	reg := NewRegistry(&config.Config{}, testClient())
	aliasClaims(reg, "svtplay", "127.0.0.1")
	if ex, page := reg.identify(t.Context(), mustParse(t, srv.URL+"/video/a1b2c3d/a-programme")); ex.Name() != "svtplay" || page != nil {
		t.Errorf("programme link: %s, probed %v; want svtplay unprobed", ex.Name(), page != nil)
	}
	if ex, page := reg.identify(t.Context(), mustParse(t, srv.URL+"/nyheter/a-story")); ex != reg.fallback || page == nil {
		t.Errorf("news page: %s, probed %v; want the fallback after a probe", ex.Name(), page != nil)
	}
}

// TestHostClaimedFallbackReportsTheSitesOwnReason: a player's page that
// answers without its player — removed, private, blocked — is the site's
// own, so its extractor says why. It is never handed to the generic
// fallback, which would read the page's related videos as the item.
func TestHostClaimedFallbackReportsTheSitesOwnReason(t *testing.T) {
	var (
		mu    sync.Mutex
		asked []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		mu.Unlock()
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<html><head><title>EPORNER.COM</title></head><body><p>This video was removed.</p>`)
		for i := range 20 {
			fmt.Fprintf(w, `<a href="/video-Related%02dAbCd/another-clip/">related</a>`, i)
		}
		fmt.Fprint(w, `</body></html>`)
	}))
	defer srv.Close()
	reg := NewRegistry(&config.Config{}, testClient())
	aliasClaims(reg, "eporner", "127.0.0.1")
	aliasHosts(reg, "eporner", "127.0.0.1")
	res, ex, err := reg.Extract(t.Context(), srv.URL+"/video-RemovedAbCd/a-clip/", Options{})
	if err == nil || ex.Name() != "eporner" {
		t.Fatalf("result %+v from %v, error %v; want eporner's refusal", res, ex, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range asked {
		if strings.Contains(path, "Related") {
			t.Fatalf("a related video was read: %v", asked)
		}
	}
}

// TestHostClaimLeavesFilesOnTheHost: an image or media file on a claimed
// host is the file, not a page of the site's, unless its form is the
// extractor's or the extractor resolves direct links (claimsFiles).
func TestHostClaimLeavesFilesOnTheHost(t *testing.T) {
	reg := testRegistry(t, nil)
	if ex := reg.hostClaim(mustParse(t, "https://img.pixhost.to/images/1/a.jpg"), claimFallback, claimFirst); ex != nil {
		t.Errorf("an image file was claimed by %s", ex.Name())
	}
	if ex := reg.hostClaim(mustParse(t, "https://dl.dropboxusercontent.com/s/abc/a.zip"), claimFirst); ex == nil || ex.Name() != "dropbox" {
		t.Errorf("a direct dropbox link went to %v, want dropbox", ex)
	}
}

// TestFamilyInstallsAreClaimedAndLabelled: a family's known installs, and
// those HEAPLEACH_EXTRA_HOSTS adds, are claimed by the family and labelled
// by install.
func TestFamilyInstallsAreClaimedAndLabelled(t *testing.T) {
	reg := testRegistry(t, &config.Config{ExtraHosts: map[string][]string{
		config.FamilyKVS:       {"Clips.example.test"},
		config.FamilyFoolFuuka: {"archive.example.test"},
		config.FamilyFediverse: {"social.example.test"},
	}})
	for raw, want := range map[string]string{
		"https://www.porntrex.com/video/1/a-clip/":      "porntrex",
		"https://www.clips.example.test/videos/a-clip/": "clips",
		"https://ibb.co/AbCdEf1":                        "imgbb",
		"https://desuarchive.org/a/thread/1/":           "desuarchive",
		"https://archive.palanq.win/a/thread/1/":        "palanq",
		"https://archive.example.test/a/thread/1/":      "archive",
		"https://yande.re/post?tags=a_tag":              "yande",
	} {
		if got := reg.Label(mustParse(t, raw)); got != want {
			t.Errorf("Label(%s) = %q, want %q", raw, got, want)
		}
	}
	if ex := reg.hostClaim(mustParse(t, "https://www.clips.example.test/videos/a-clip/"), claimFallback); ex == nil || ex.Name() != "kvs" {
		t.Errorf("a configured KVS install is claimed by %v, want kvs as a fallback", ex)
	}
	if ex := reg.hostClaim(mustParse(t, "https://social.example.test/@alice"), claimFirstForm); ex == nil || ex.Name() != "fediverse" {
		t.Errorf("a configured fediverse server is claimed by %v, want fediverse first", ex)
	}
	if ex := reg.hostClaim(mustParse(t, "https://archive.example.test/a/thread/1/"), claimFirstForm); ex == nil || ex.Name() != "foolfuuka" {
		t.Errorf("a configured archive is claimed by %v, want foolfuuka first", ex)
	}
}

// TestAnUnrelatedPageIsNotBlamedOnASite: a page on an unrelated host whose
// path only looks like one site's link (/watch/<slug>.html) is that host's
// own page: neither labelled as that site nor refused in its name.
func TestAnUnrelatedPageIsNotBlamedOnASite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, `<html><title>A blog post</title><p>Nothing to download.</p></html>`)
	}))
	defer srv.Close()
	reg := NewRegistry(&config.Config{}, testClient())
	u := srv.URL + "/watch/a-clip.html"
	if got := reg.Label(mustParse(t, u)); got != "127.0.0.1" {
		t.Errorf("label %q, want the host", got)
	}
	_, _, err := reg.Extract(t.Context(), u, Options{})
	if err == nil || !strings.Contains(err.Error(), "is a web page, not a file") {
		t.Errorf("error %v, want the plain-page refusal", err)
	}
}

// TestBroadcasterExplainsAPageNothingReads: a broadcaster's page outside its
// programme forms is recognised like any page, and a news article with a
// clip is the generic fallback's; but a page the fallback finds nothing on
// is the broadcaster's, refused in its words rather than as a web page.
func TestBroadcasterExplainsAPageNothingReads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		if strings.Contains(r.URL.Path, "clip") {
			fmt.Fprint(w, `<html><title>A story</title><video src="https://media.example.test/story.mp4"></video></html>`)
			return
		}
		fmt.Fprint(w, `<html><title>A category</title><p>Programmes.</p></html>`)
	}))
	defer srv.Close()
	reg := NewRegistry(&config.Config{}, testClient())
	aliasClaims(reg, "svtplay", "127.0.0.1")
	res, ex, err := reg.Extract(t.Context(), srv.URL+"/nyheter/a-clip-story", Options{})
	if err != nil || ex != reg.fallback || len(res.Files) != 1 {
		t.Errorf("a story with a clip: %+v from %v, %v; want the fallback's file", res, ex, err)
	}
	_, ex, err = reg.Extract(t.Context(), srv.URL+"/kategori/a-category", Options{})
	if err == nil || ex == nil || ex.Name() != "svtplay" || errors.Is(err, errPlainPage) {
		t.Errorf("a page nothing reads: %v from %v; want svtplay's own refusal", err, ex)
	}
	if got := reg.Label(mustParse(t, srv.URL+"/kategori/a-category")); got != "svtplay" {
		t.Errorf("label %q, want svtplay", got)
	}
}
