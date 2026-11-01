// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// TestVimeoPlayerProbeIsNotStarvedByAdmission: the player page a watch page
// without meta tags is taken by is asked within the probe's own bound once
// admitted, so a discovery queue that keeps every slot busy longer than the
// bound does not leave a false "not Vimeo" kept on the shared page.
func TestVimeoPlayerProbeIsNotStarvedByAdmission(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprintf(w, `<html><script>%s{}</script></html>`, vimeoConfigMarker)
	}))
	t.Cleanup(srv.Close)
	ctx := httpx.WithAdmission(context.Background(), httpx.NewAdmission(1))
	release, err := httpx.Admit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old := platformProbeTimeout
	platformProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { platformProbeTimeout = old })
	go func() {
		time.Sleep(3 * platformProbeTimeout)
		release()
	}()
	p := htmlPage(t, "https://vimeo.example.test/111", `<html><head><title>A Film on Vimeo</title>`+
		`<meta property="og:site_name" content="Vimeo"></head></html>`)
	if !vimeoWith(srv).Recognise(ctx, p) {
		t.Fatal("the player probe timed out waiting for admission rather than on the network")
	}
}
