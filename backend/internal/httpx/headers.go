// SPDX-License-Identifier: MIT

package httpx

import (
	"maps"
	"net/http"
)

// Header is a set of request headers to apply on top of the client defaults.
// Extractors return one per file so the downloader can replay exactly what a
// host needs on every attempt.
type Header map[string]string

// Header names. net/http exports no constants for these, and they are spelled
// out across every extractor, so they are collected here to keep the spelling
// honest and greppable.
const (
	HeaderAccept             = "Accept"
	HeaderAcceptEncoding     = "Accept-Encoding"
	HeaderAcceptLanguage     = "Accept-Language"
	HeaderAcceptRanges       = "Accept-Ranges"
	HeaderAuthorization      = "Authorization"
	HeaderCFMitigated        = "Cf-Mitigated"
	HeaderContentDisposition = "Content-Disposition"
	HeaderContentRange       = "Content-Range"
	HeaderContentType        = "Content-Type"
	HeaderCookie             = "Cookie"
	HeaderETag               = "ETag"
	HeaderIfRange            = "If-Range"
	HeaderLastModified       = "Last-Modified"
	HeaderOrigin             = "Origin"
	HeaderRange              = "Range"
	HeaderReferer            = "Referer"
	HeaderRequestedWith      = "X-Requested-With"
	HeaderRetryAfter         = "Retry-After"
	HeaderUserAgent          = "User-Agent"

	// Fetch metadata headers. Browsers always send these, and some hosts
	// treat their absence as a bot signal.
	HeaderSecFetchDest = "Sec-Fetch-Dest"
	HeaderSecFetchMode = "Sec-Fetch-Mode"
	HeaderSecFetchSite = "Sec-Fetch-Site"

	// Response headers, most of them set by the server on what it serves.
	HeaderAccelBuffering        = "X-Accel-Buffering"
	HeaderAllow                 = "Allow"
	HeaderCacheControl          = "Cache-Control"
	HeaderContentEncoding       = "Content-Encoding"
	HeaderContentLength         = "Content-Length"
	HeaderContentSecurityPolicy = "Content-Security-Policy"
	HeaderContentTypeOptions    = "X-Content-Type-Options"
	HeaderFrameOptions          = "X-Frame-Options"
	HeaderReferrerPolicy        = "Referrer-Policy"
	HeaderVary                  = "Vary"
)

// Common header values.
const (
	AcceptAny  = "*/*"
	AcceptJSON = "application/json, text/plain, */*"
	AcceptHTML = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

	ContentTypeJSON = "application/json"
	ContentTypeForm = "application/x-www-form-urlencoded"

	// RequestedWithXHR marks a request the page would make from script.
	// Some players answer their own endpoints only to it.
	RequestedWithXHR = "XMLHttpRequest"

	// EncodingIdentity asks for the raw bytes. A transparently compressed
	// body would make the progress total disagree with what lands on disk.
	EncodingIdentity = "identity"
)

// Navigate is what a browser sends with a page it is navigating to: it asks
// for HTML, and says so in the fetch metadata some hosts check. It is shared:
// apply it, or Merge it into a header of your own, but never write to it.
var Navigate = Header{
	HeaderAccept:       AcceptHTML,
	HeaderSecFetchDest: "document",
	HeaderSecFetchMode: "navigate",
}

// Apply sets each entry on the request, overriding what is there: the
// client's defaults, or what an earlier Apply set.
func (h Header) Apply(req *http.Request) {
	for name, value := range h {
		req.Header.Set(name, value)
	}
}

// Merge returns a copy of base with the entries of extra applied over it.
// Either may be nil.
func (h Header) Merge(extra Header) Header {
	out := make(Header, len(h)+len(extra))
	maps.Copy(out, h)
	maps.Copy(out, extra)
	return out
}

// Referer builds the single-entry header map most hosts need.
func Referer(url string) Header {
	return Header{HeaderReferer: url}
}

// RefererOrigin builds the Referer/Origin pair that CORS-guarded endpoints
// expect from a real browser.
func RefererOrigin(referer, origin string) Header {
	return Header{HeaderReferer: referer, HeaderOrigin: origin}
}

// SiteOrigin is RefererOrigin for a request made from a site's front page:
// root is the scheme and host, without a trailing slash.
func SiteOrigin(root string) Header {
	return RefererOrigin(root+"/", root)
}
