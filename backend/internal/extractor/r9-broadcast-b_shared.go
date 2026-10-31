// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Steps several broadcasters repeat around expandSeries: a listing read
// off the rendered page where the catalogue has none, a GraphQL request,
// a Next.js page's island, and the probes that tell a region-only
// programme refused here from one that plays.

// pageIDs reads the episodes a rendered page links to, by the id in link's
// first group, for a broadcaster whose catalogue does not describe the odd
// listing. site opens its errors; doc is the page, for its title.
func pageIDs(ctx context.Context, client *httpx.Client, u *url.URL, headers httpx.Header, site string, link *regexp.Regexp) (doc string, episodes []broadcastEpisode, err error) {
	doc, _, err = fetchDocument(ctx, client, u, headers)
	if err != nil {
		return "", nil, fmt.Errorf("%s: fetch %s: %w", site, u.Redacted(), err)
	}
	for _, id := range matchedIDs(doc, link) {
		episodes = append(episodes, broadcastEpisode{id: id})
	}
	if len(episodes) == 0 {
		return "", nil, fmt.Errorf("%s: no episodes listed on %s", site, u.Redacted())
	}
	return doc, episodes, nil
}

// postGraphQL asks a GraphQL endpoint query, with its variables, into out.
func postGraphQL(ctx context.Context, client *httpx.Client, endpoint string, headers httpx.Header, query string, variables, out any) error {
	return client.PostJSON(ctx, endpoint, headers, map[string]any{"query": query, "variables": variables}, out)
}

// regionGate leaves out a listing's items shown in one region alone (only)
// once one of them is refused here. refusedHere asks of them in the
// listing's order, at most probes of them, until one can tell (known), and
// its answer holds for the rest: a refusal says nothing of the programme
// and everything of the address asking. It returns the items kept, in
// order, and how many were left out.
func regionGate[T any](ctx context.Context, items []T, only func(T) bool, probes int, refusedHere func(context.Context, T) (refused, known bool)) ([]T, int) {
	blocked := false
	for _, item := range items {
		if probes == 0 {
			break
		}
		if !only(item) {
			continue
		}
		probes--
		if refused, known := refusedHere(ctx, item); known {
			blocked = refused
			break
		}
	}
	if !blocked {
		return items, 0
	}
	kept := make([]T, 0, len(items))
	for _, item := range items {
		if !only(item) {
			kept = append(kept, item)
		}
	}
	return kept, len(items) - len(kept)
}

// refusesByte asks for a file's first byte, to learn whether its host
// serves it to this address at all: a 403 is a refusal, and a probe that
// could not be made is none.
func refusesByte(ctx context.Context, client *httpx.Client, link string, headers httpx.Header) bool {
	status, err := client.StatusOnce(ctx, http.MethodGet, link,
		headers.Merge(httpx.Header{httpx.HeaderRange: "bytes=0-0"}))
	return err == nil && status == http.StatusForbidden
}

// nextDataScript is the JSON island a Next.js page renders from.
var nextDataScript = regexp.MustCompile(`(?s)<script[^>]*\bid="__NEXT_DATA__"[^>]*>(.*?)</script>`)

// nextData is the JSON island of a page rendered by Next.js
// (<script id="__NEXT_DATA__">), as written, or false when it has none.
func nextData(doc string) (string, bool) {
	if m := nextDataScript.FindStringSubmatch(doc); m != nil {
		return m[1], true
	}
	return "", false
}

// allDigits reports a non-empty run of decimal digits: an id where a slug
// could stand instead.
func allDigits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}
