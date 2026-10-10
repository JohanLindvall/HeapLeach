// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// platformJSON is an optional identification request, not an extraction.
// Guesses get one bounded attempt, including on 429. The caller must verify
// the platform's response shape before handing it to an extractor; a 200 or
// valid JSON alone says nothing. Once identified, extraction uses its normal
// retries, pacing and error handling.
func platformJSON(ctx context.Context, client *httpx.Client, endpoint string, headers httpx.Header, out any) bool {
	ctx, cancel := context.WithTimeout(ctx, config.PlatformProbeTimeout)
	defer cancel()
	req, err := client.NewRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	req.Header.Set(httpx.HeaderAccept, httpx.ContentTypeJSON)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.DoOnce(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get(httpx.HeaderContentType))
	if mediaType != "" && mediaType != httpx.ContentTypeJSON && !strings.HasSuffix(mediaType, "+json") &&
		mediaType != "text/plain" && mediaType != "text/css" {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, config.MaxResponseBytes+1))
	return err == nil && len(body) <= config.MaxResponseBytes && json.Unmarshal(body, out) == nil
}

// Paths select plausible APIs only after the page's own recognised content
// has had its turn. In particular /post is common on ordinary blogs, and a
// working embedded album there needs no guessed booru or archive requests.
// These paths never enter Registry.Known: doing so would turn navigation
// links into recursive extraction.
func platformAPIs(ctx context.Context, client *httpx.Client, u *url.URL, opts Options) (*Result, error) {
	for _, sniff := range []directSniff{pixeldrainSniff, kemonoSniff, foolFuukaSniff, booruSniff, mediaWikiSniff} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if res, err := sniff(ctx, client, u, opts); res != nil || err != nil {
			return res, err
		}
	}
	return nil, ctx.Err()
}

// platformPage identifies software in the document already fetched by the
// fallback. This covers arbitrary article and listing paths without claiming
// those paths in the registry or fetching a guessed listing on another site.
func platformPage(ctx context.Context, client *httpx.Client, u *url.URL, root *html.Node, doc string, opts Options) (*Result, error) {
	generator := strings.ToLower(metaContent(root, "generator"))
	if kvsFingerprint(root, doc, generator) {
		k := &KVS{client: client, host: u.Hostname()}
		return k.extract(ctx, u, opts, doc)
	}
	if strings.HasPrefix(generator, "mediawiki") {
		var api string
		// EditURI advertises api.php even when the article URL gives no
		// indication of the script directory. Keep the probe on this site.
		if link := findFirst(root, func(n *html.Node) bool {
			return n.DataAtom == atom.Link && strings.EqualFold(attr(n, "rel"), "EditURI")
		}); link != nil {
			if ref, err := url.Parse(attr(link, "href")); err == nil {
				ref = u.ResolveReference(ref)
				if util.Origin(ref) == util.Origin(u) {
					ref.RawQuery, ref.Fragment = "", ""
					api = ref.String()
				}
			}
		}
		res, err := mediaWikiDetect(ctx, client, u, opts, api)
		if res == nil && err == nil {
			err = fmt.Errorf("mediawiki: %s identifies as MediaWiki but no API answered", u.Hostname())
		}
		return res, err
	}
	if strings.HasPrefix(generator, "foolfuuka") {
		f := &FoolFuuka{client: client, site: foolFuukaSite{root: util.Origin(u), name: foolFuukaLabel(u.Hostname())}}
		return f.Extract(ctx, u, opts)
	}
	for _, family := range []struct {
		name string
		api  booruAPI
	}{
		{"danbooru", apiDanbooru}, {"e621", apiE621}, {"moebooru", apiMoebooru},
		{"gelbooru 0.1", apiGelbooru01}, {"gelbooru", apiGelbooru},
		{"philomena", apiPhilomena}, {"twibooru", apiTwibooru}, {"szurubooru", apiSzurubooru},
	} {
		if strings.HasPrefix(generator, family.name) {
			return NewBooru(client).extract(ctx, u, &booruSite{name: u.Hostname(), root: util.Origin(u), api: family.api})
		}
	}
	return nil, nil
}

// A generic flashvars object or a videos container is insufficient. KVS
// identifies itself through the licensed player or its asynchronous block
// controls, which carry both the block id and the platform's parameters.
func kvsFingerprint(root *html.Node, doc, generator string) bool {
	if generator == "kvs" || strings.HasPrefix(generator, "kernel video sharing") {
		return true
	}
	if vars, err := parseFlashvars(doc); err == nil && vars["license_code"] != "" {
		return true
	}
	return findFirst(root, func(n *html.Node) bool {
		if n.DataAtom == atom.Script {
			return strings.Contains(attr(n, "src"), "/kt_player.") || strings.Contains(textOf(n), "kt_player(")
		}
		return attr(n, "data-action") == "ajax" &&
			strings.HasPrefix(attr(n, "data-block-id"), "list_videos") && attr(n, "data-parameters") != ""
	}) != nil
}
