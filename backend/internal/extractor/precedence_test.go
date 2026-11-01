// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Registration order decides between families whose recognisers both take
// a page, and deterministic construction alone does not prove which: this
// pins it on pages both would take. A PeerTube channel is a fediverse
// community by its path (/c/<name>), and a PeerTube server publishes
// nodeinfo; the narrower family, PeerTube, is registered first and is the
// one chosen.
func TestRegistryPrefersTheNarrowerFamily(t *testing.T) {
	srv := fediServer(t, "peertube", func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/c/a_channel" {
			return false
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		_, _ = io.WriteString(w, peerTubeClientPage)
		return true
	})
	client := testClient()
	reg := NewRegistry(&config.Config{}, client)
	u := mustParse(t, srv.URL+"/c/a_channel")

	page := probePage(context.Background(), client, u)
	var takers []string
	for _, e := range reg.extractors {
		if r, ok := e.(Recogniser); ok && r.Recognise(context.Background(), page) {
			takers = append(takers, e.Name())
		}
	}
	if len(takers) < 2 || takers[0] != "peertube" {
		t.Fatalf("recognised by %v, want peertube first among several", takers)
	}
	if ex, _ := reg.identify(context.Background(), u); ex == nil || ex.Name() != "peertube" {
		t.Errorf("identified as %v, want peertube ahead of %v", ex, takers[1:])
	}
}
