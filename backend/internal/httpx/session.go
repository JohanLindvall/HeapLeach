// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"net/http"
)

type cookieJarContextKey struct{}

// WithCookieJar isolates a source's cookies while retaining the client's
// transport, timeouts and selected route. Redirects and retries use this jar
// too; neither the shared client nor a leased route's session is changed.
func WithCookieJar(ctx context.Context, jar http.CookieJar) context.Context {
	return context.WithValue(ctx, cookieJarContextKey{}, jar)
}
