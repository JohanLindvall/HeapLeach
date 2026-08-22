package extractor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A thread as the API answers it: the opening post with an attachment and a
// subject, a text-only reply, and a reply with an attachment. tim is the
// stored name's stem; posts without one carry no file.
const fourChanThread = `{"posts":[
  {"no":570368,"sub":"A Thread","tim":1735000000001,"filename":"original","ext":".png","fsize":123456},
  {"no":570369,"com":"text only"},
  {"no":570370,"tim":1735000000002,"filename":"original","ext":".webm","fsize":7890}
]}`

func TestFourChanExtract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/po/thread/570368.json" {
			t.Errorf("path = %q, want /po/thread/570368.json", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		_, _ = w.Write([]byte(fourChanThread))
	}))
	t.Cleanup(srv.Close)

	f := NewFourChan(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	f.api = srv.URL
	f.files = "https://files.example.test"

	u, _ := ParseURL("https://boards.4chan.org/po/thread/570368")
	res, err := f.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "A Thread" {
		t.Errorf("title = %q, want the thread subject", res.Title)
	}
	if len(res.Files) != 2 {
		t.Fatalf("got %d files, want 2 (the text-only post carries none)", len(res.Files))
	}
	// The stored name keeps posts ordered and unique; two posters uploading
	// "original.png" is the ordinary case.
	if res.Files[0].Name != "1735000000001.png" {
		t.Errorf("name = %q, want the stored name", res.Files[0].Name)
	}
	if want := "https://files.example.test/po/1735000000001.png"; res.Files[0].URL != want {
		t.Errorf("url = %q, want %q", res.Files[0].URL, want)
	}
	if res.Files[0].Size != 123456 {
		t.Errorf("size = %d, want the exact fsize", res.Files[0].Size)
	}
}

func TestFourChanRejectsNonThreadLinks(t *testing.T) {
	f := NewFourChan(nil)
	u, _ := ParseURL("https://boards.4chan.org/po/")
	if _, err := f.Extract(context.Background(), u, Options{}); err == nil ||
		!strings.Contains(err.Error(), "thread") {
		t.Errorf("err = %v, want a message naming the expected shape", err)
	}
}
