// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestArteCollectionKeepsTheSeasonsItCanRead pins a collection whose
// seasons do not all load: a later season that fails is left out with the
// note saying so, but the first failing fails the collection, nothing read
// before it to stand for the rest.
func TestArteCollectionKeepsTheSeasonsItCanRead(t *testing.T) {
	playlists := map[string]string{
		"RC-000001": `{"data":{"attributes":{"metadata":{"title":"A Series"},"items":[
			{"providerId":"RC-000002","title":"Season One"},{"providerId":"RC-000003","title":"Season Two"}]}}}`,
		"RC-000004": `{"data":{"attributes":{"metadata":{"title":"A Series"},"items":[
			{"providerId":"RC-000003","title":"Season Two"},{"providerId":"RC-000002","title":"Season One"}]}}}`,
		"RC-000002": `{"data":{"attributes":{"metadata":{"title":"Season One"},"items":[
			{"providerId":"100001-001-A","title":"First Part"}]}}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch {
		case strings.HasPrefix(r.URL.Path, "/config/"):
			fmt.Fprint(w, `{}`)
		case playlists[id] != "":
			fmt.Fprint(w, playlists[id])
		default:
			http.Error(w, "down", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	a := NewArte(testClient())
	a.api = srv.URL

	res, err := a.collection(context.Background(), "en", "RC-000001", Options{}.episodeBudget(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Dir != "Season One" || res.Files[0].Name != "First Part.mp4" ||
		res.Title != "A Series" || res.Note != unreadableSeasons {
		t.Errorf("collection = %+v", res)
	}

	if res, err := a.collection(context.Background(), "en", "RC-000004", Options{}.episodeBudget(), 0); err == nil {
		t.Errorf("first season failing = %+v, want an error", res)
	}
}
