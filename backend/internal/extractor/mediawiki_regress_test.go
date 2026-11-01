// SPDX-License-Identifier: MIT

package extractor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// mediaWikiBatches serves one listing's batches in turn, each continued to
// the next by its number.
func mediaWikiBatches(t *testing.T, batches ...string) mediaWikiSite {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := 0
		if c := r.URL.Query().Get("batch"); c != "" {
			n = int(c[0] - '0')
		}
		cont := ""
		if n+1 < len(batches) {
			cont = `"continue":{"batch":"` + string(rune('0'+n+1)) + `"},`
		}
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		_, _ = io.WriteString(w, `{`+cont+`"query":{"pages":[`+batches[n]+`]}}`)
	}))
	t.Cleanup(srv.Close)
	return mediaWikiSite{api: srv.URL + "/api.php", name: "A Wiki"}
}

func mediaWikiRedLink(title string) string { return `{"title":"` + title + `","missing":true}` }

func mediaWikiFileJSON(title string) string {
	name := strings.TrimPrefix(title, "File:")
	return `{"title":"` + title + `","imageinfo":[{"url":"https://upload.example.test/a/` + name + `","size":10}]}`
}

// The file cap counts files. Red links in a batch, and files another listing
// of the same walk already took, use none of it.
func TestMediaWikiListBudgetCountsFiles(t *testing.T) {
	site := mediaWikiBatches(t,
		strings.Join([]string{mediaWikiRedLink("File:Gone1.jpg"), mediaWikiRedLink("File:Gone2.jpg"),
			mediaWikiFileJSON("File:Taken.jpg"), mediaWikiFileJSON("File:First.jpg")}, ","),
		strings.Join([]string{mediaWikiFileJSON("File:Second.jpg"), mediaWikiFileJSON("File:Third.jpg")}, ","),
	)
	files, note, err := testMediaWiki(t).list(t.Context(), site, url.Values{}, 2, map[string]bool{"File:Taken.jpg": true})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if strings.Join(names, " ") != "First.jpg Second.jpg" || note != "partial — file limit" {
		t.Errorf("files %v note %q; want the first two files not yet taken and the cap noted", names, note)
	}
}

// A page whose file arrives in a later batch, when the first ran out of
// room for its imageinfo, is the file, not a repeat of the red link it first
// looked like.
func TestMediaWikiListTakesAFileHandedOverLater(t *testing.T) {
	site := mediaWikiBatches(t,
		`{"title":"File:Later.jpg"}`,
		mediaWikiFileJSON("File:Later.jpg"),
	)
	files, _, err := testMediaWiki(t).list(t.Context(), site, url.Values{}, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "Later.jpg" {
		t.Errorf("files %+v, want the one file handed over in the second batch", files)
	}
}
