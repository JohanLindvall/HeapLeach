// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A stripped render keeps the generator and the API it advertises but not
// ResourceLoader: the advertised API on the page's own origin identifies
// it, at any article path. The generator alone cannot identify the wiki
// when its conventional API paths do not answer.
func TestMediaWikiRecognisesAStrippedRender(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/api.php" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		_, _ = io.WriteString(w, mediaWikiSiteInfo)
	}))
	t.Cleanup(srv.Close)
	m := NewMediaWiki(testClient())
	stripped := `<html><head><title>An article - A Wiki</title><meta name="generator" content="MediaWiki 1.43">` +
		`<link rel="EditURI" type="application/rsd+xml" href="/custom/api.php?action=rsd"></head><body></body></html>`
	if !m.Recognise(context.Background(), htmlPage(t, srv.URL+"/An_article", stripped)) {
		t.Error("a stripped render advertising its API was not recognised")
	}
	bare := `<html><head><title>An article - A Wiki</title><meta name="generator" content="MediaWiki 1.43"></head><body></body></html>`
	if m.Recognise(context.Background(), htmlPage(t, srv.URL+"/An_article", bare)) {
		t.Error("the generator alone was taken")
	}
}
