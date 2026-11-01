// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// The per-site bounds hold a listing to what it always read, below the
// shared page cap, whatever the user's file cap would allow. Each server
// here goes on for ever, so only the bound ends the walk.

func TestKemonoCreatorStopsAtItsPageBound(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sample/user/creator/profile":
			fmt.Fprint(w, `{"id":"creator","service":"sample","name":"A Creator"}`)
		case "/api/v1/sample/user/creator/posts":
			asked.Add(1)
			from, _ := strconv.Atoi(r.URL.Query().Get("o"))
			posts := make([]kemonoPost, 0, kemonoPageSize)
			for i := from; i < from+kemonoPageSize; i++ {
				posts = append(posts, kemonoPost{ID: strconv.Itoa(i), File: kemonoAttachment{Path: fmt.Sprintf("/a/%d.jpg", i)}})
			}
			_ = json.NewEncoder(w).Encode(posts)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	res := extractOK(t, NewKemono(testClient()), srv.URL+"/sample/user/creator", Options{})
	if n := int(asked.Load()); n != config.KemonoMaxPages || len(res.Files) != config.KemonoMaxPages*kemonoPageSize || res.Note != "partial — page limit" {
		t.Errorf("asked %d pages, %d files, note %q; want %d pages and the page limit", n, len(res.Files), res.Note, config.KemonoMaxPages)
	}
}

func TestRedGifsUserStopsAtItsPageBound(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/temporary" {
			fmt.Fprint(w, `{"token":"t"}`)
			return
		}
		asked.Add(1)
		page := r.URL.Query().Get("page")
		fmt.Fprintf(w, `{"pages":1000,"gifs":[{"id":"clip%s","urls":{"hd":"https://media.example.test/clip%s.mp4"}}]}`, page, page)
	}))
	defer srv.Close()
	r := NewRedGifs(testClient())
	r.api = srv.URL
	res, err := r.user(t.Context(), "someone", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := int(asked.Load()); n != config.RedGifsMaxPages || len(res.Files) != config.RedGifsMaxPages || res.Note != "partial — page limit" {
		t.Errorf("asked %d pages, %d files, note %q; want %d and the page limit", n, len(res.Files), res.Note, config.RedGifsMaxPages)
	}
}

func TestEromeProfileStopsAtItsPageBound(t *testing.T) {
	var asked atomic.Int32
	e, base := eromeWalkServer(t, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		page := cmpOrOne(r.URL.Query().Get("page"))
		fmt.Fprintf(w, `<html><head><title>creator - EroMe</title></head><body><a href="https://www.erome.com/a/P%sA">One</a></body></html>`, page)
	})
	albums, _, note, err := e.profileAlbums(t.Context(), mustParse(t, base+"/creator"), Options{limits: Limits{Sources: 1000}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := int(asked.Load()); n != config.EromeMaxProfilePages || len(albums) != config.EromeMaxProfilePages || note != "partial — page limit" {
		t.Errorf("asked %d pages, %d albums, note %q; want %d and the page limit", n, len(albums), note, config.EromeMaxProfilePages)
	}
}

// cmpOrOne is a page number as a listing names it, its first page bare.
func cmpOrOne(page string) string {
	if page == "" {
		return "1"
	}
	return page
}

func TestEpornerProfileStopsAtItsPageBound(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		page := strings.Trim(strings.TrimPrefix(r.URL.Path, "/profile/Someone/uploaded-videos"), "/")
		page = cmpOrOne(page)
		fmt.Fprintf(w, `<html><body><span>Uploaded videos (100000) </span><div class="streamevents">
			<div class="mb"><p class="mbtit"><a href="/video-Page%sClip/a-clip/">A Clip</a></p></div></div></body></html>`, page)
	}))
	defer srv.Close()
	res := extractOK(t, epornerUnderTest(t), srv.URL+"/profile/Someone/uploaded-videos/", Options{})
	want := fmt.Sprintf("partial — page limit; %d of 100000", config.EpornerMaxProfilePages)
	if n := int(asked.Load()); n != config.EpornerMaxProfilePages || len(res.Files) != config.EpornerMaxProfilePages || res.Note != want {
		t.Errorf("asked %d pages, %d files, note %q; want %d and %q", n, len(res.Files), res.Note, config.EpornerMaxProfilePages, want)
	}
}

func TestBandzoogleStopsAtItsContinuationBound(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		fmt.Fprint(w, "<html><body>"+bandzoogleList(strconv.Itoa(offset+1), "true", strconv.Itoa(offset+100))+"</body></html>")
	}))
	defer srv.Close()
	u := mustParse(t, srv.URL+"/music")
	root := mustParseDoc(t, "<html><head><title>A Band Name</title></head><body>"+bandzoogleList("1", "true", "1")+"</body></html>")
	res, err := bandzoogleRead(t.Context(), testClient(), root, u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := int(asked.Load()); n != config.BandzoogleMaxContinuations || len(res.Files) != 1+config.BandzoogleMaxContinuations || res.Note != "partial — page limit" {
		t.Errorf("asked %d continuations, %d tracks, note %q; want %d and the page limit", n, len(res.Files), res.Note, config.BandzoogleMaxContinuations)
	}
}

// A shared directory is held to its own file bound when the user's file
// limit is higher, said as the listing limit; a lower file limit cuts first.
func TestPixeldrainBounds(t *testing.T) {
	if b := pixeldrainBounds(Options{}); b.files != config.PixeldrainMaxFiles || b.filesReason != "listing limit" || b.folders != config.MaxTreeFolders {
		t.Errorf("default bounds %+v", b)
	}
	if b := pixeldrainBounds(Options{limits: Limits{Files: 7}}); b.files != 7 || b.filesReason != fileLimit {
		t.Errorf("lower file limit: %+v", b)
	}
}

// A tree walk stops at its host's file bound, in the host's words, and
// reads no more folders than the host's folder bound.
func TestWalkTreeWithinAHostsBounds(t *testing.T) {
	type dir struct{ name string }
	list := func(_ context.Context, _ *treeWalk, d dir, _ int) ([]treeItem[dir], error) {
		var items []treeItem[dir]
		for i := range 3 {
			items = append(items, treeItem[dir]{File: &File{Name: fmt.Sprintf("%s-%d.bin", d.name, i)}})
		}
		for i := range 3 {
			child := fmt.Sprintf("%s/%d", d.name, i)
			items = append(items, treeItem[dir]{Folder: dir{child}, Key: child, Name: strconv.Itoa(i)})
		}
		return items, nil
	}
	res := &Result{}
	if err := walkTreeWithin(t.Context(), Options{}, treeBounds{files: 5, filesReason: "listing limit", folders: 100}, res, "root", dir{"root"}, list); err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 5 || res.Note != "partial — listing limit" {
		t.Errorf("%d files, note %q; want 5 and the listing limit", len(res.Files), res.Note)
	}
	res = &Result{}
	if err := walkTreeWithin(t.Context(), Options{}, treeBounds{files: 1000, filesReason: fileLimit, folders: 2}, res, "root", dir{"root"}, list); err != nil {
		t.Fatal(err)
	}
	// The root and one subfolder are read; the rest are past the bound.
	if len(res.Files) != 6 || res.Note != "partial — folder limit" {
		t.Errorf("%d files, note %q; want 6 and the folder limit", len(res.Files), res.Note)
	}
}

// A filester folder walk reads no more folders than the user's source
// limit allows, the root among them.
func TestFilesterFolderWalkHonoursTheSourceLimit(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		name := strings.TrimPrefix(r.URL.Path, "/f/")
		fmt.Fprint(w, filesterTestListing(filesterTestCard("c"+name, "File "+name+".mp4", "100"), "")+
			`<div id="subfoldersGrid">`+
			`<a class="subfolder-item" href="/f/`+name+`a"><span class="subfolder-name">A</span></a>`+
			`<a class="subfolder-item" href="/f/`+name+`b"><span class="subfolder-name">B</span></a></div>`)
	}))
	defer srv.Close()
	res := extractOK(t, NewFilester(testClient()), srv.URL+"/f/root", Options{limits: Limits{Sources: 3}})
	if n := asked.Load(); n != 3 || len(res.Files) != 3 || res.Note != "partial — folder limit" {
		t.Errorf("asked %d folders, %d files, note %q; want 3 and the folder limit", n, len(res.Files), res.Note)
	}
}

// The broadcasters (NRK, SVT, RÚV, RaiPlay, ABC listen, PBS) that always
// resolved at most this many episodes of a
// programme keep that bound, below the shared one.
func TestStrandEpisodeBudget(t *testing.T) {
	if config.MaxResolvedEpisodesStrand != 500 || config.MaxResolvedEpisodesStrand >= config.MaxResolvedEpisodes {
		t.Errorf("strand budget %d, shared %d", config.MaxResolvedEpisodesStrand, config.MaxResolvedEpisodes)
	}
}
