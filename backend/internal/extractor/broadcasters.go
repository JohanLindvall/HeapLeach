// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"net/url"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// broadcastPage is fetchDocument for a link held as a string, as the
// broadcasters' page readers pass them: the page the registry probed is
// reused when the link is the one submitted, and fetched otherwise.
func broadcastPage(ctx context.Context, client *httpx.Client, link string, headers httpx.Header) (string, error) {
	u, err := url.Parse(link)
	if err != nil {
		return client.GetString(ctx, link, headers)
	}
	doc, _, err := fetchDocument(ctx, client, u, headers)
	return doc, err
}
