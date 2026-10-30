// SPDX-License-Identifier: MIT

package extractor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestFapelloFullSize(t *testing.T) {
	cases := map[string]string{
		// The listing thumbnail differs from the file by a _300px suffix.
		"https://fapello.com/content/a/b/model/1000/model_0001_300px.jpg": "https://fapello.com/content/a/b/model/1000/model_0001.jpg",
		// A link that is already full size passes through unchanged.
		"https://fapello.com/content/a/b/model/1000/model_0001.jpg": "https://fapello.com/content/a/b/model/1000/model_0001.jpg",
	}
	for in, want := range cases {
		if got := fapelloFullSize(in); got != want {
			t.Errorf("fapelloFullSize(%q) = %q, want %q", in, got, want)
		}
	}
}

// The listing mixes the model's own media with recommendations for others,
// and paging past the end wraps back to the first page — so the walk keeps
// only paths naming the model, and stops on a page that adds nothing new.
func TestFapelloExtractWalksAndDeduplicates(t *testing.T) {
	page := `<html><body>
	  <a href="#"><img src="https://cdn.fapello.com/content/m/o/model-one/1000/model-one_0300_300px.jpg"></a>
	  <a href="#"><img src="https://cdn.fapello.com/content/m/o/model-one/1000/model-one_0299_300px.jpg"></a>
	  <a href="#"><img src="https://cdn.fapello.com/content/o/t/other-model/1000/other-model_0001_300px.jpg"></a>
	</body></html>`

	var pagesServed int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pagesServed++
		// Every page answers with the same listing, which is exactly what the
		// live site does when paged past its end.
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)

	res := extractOK(t, NewFapello(httpx.New("test-agent", "en-US", 0, 5*time.Second)), srv.URL+"/model-one/", Options{})
	if res.Title != "model-one" {
		t.Errorf("title = %q, want the model's name", res.Title)
	}
	if len(res.Files) != 2 {
		t.Fatalf("got %d files, want the model's own 2 (the other model's dropped)", len(res.Files))
	}
	if res.Files[0].Name != "model-one_0300.jpg" || res.Note != "" {
		t.Errorf("name = %q, want the thumbnail suffix stripped", res.Files[0].Name)
	}
	if pagesServed != 2 {
		t.Errorf("served %d pages, want 2: the repeat page ends the walk", pagesServed)
	}
}

func TestFapelloRecognise(t *testing.T) {
	f := NewFapello(nil)
	page := `<html><head><title>A Model Nude - Fapello</title></head><body>` +
		`<img src="https://cdn.example.test/content/a/-/a-model/1000/a-model_0001_300px.jpg"></body></html>`
	expectRecognised(t, f, htmlPage(t, "https://models.example.test/a-model/", page), true)
	expectRecognised(t, f, htmlPage(t, "https://models.example.test/a-model/12/", page), true)
	// The right form and the name, but only another model's media.
	expectRecognised(t, f, htmlPage(t, "https://models.example.test/b-model/", page), false)
	expectRecognised(t, f, htmlPage(t, "https://models.example.test/a-model/",
		`<html><head><title>A Model - Fapello</title></head><body></body></html>`), false)
	// The model's media on a page of another form, and without the name.
	expectRecognised(t, f, htmlPage(t, "https://models.example.test/a-model/videos/x/", page), false)
	expectRecognised(t, f, htmlPage(t, "https://models.example.test/a-model/",
		strings.Replace(page, "Fapello", "A blog", 1)), false)

	expectShape(t, f, "https://models.example.test/a-model/12/", true)
	expectShape(t, f, "https://models.example.test/a-model/", false)
	expectShape(t, f, "https://models.example.test/a-model/12", false)
}
