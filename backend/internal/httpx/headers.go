package httpx

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
	HeaderRetryAfter         = "Retry-After"
	HeaderUserAgent          = "User-Agent"

	// Fetch metadata headers. Browsers always send these, and some hosts
	// treat their absence as a bot signal.
	HeaderSecFetchDest = "Sec-Fetch-Dest"
	HeaderSecFetchMode = "Sec-Fetch-Mode"
	HeaderSecFetchSite = "Sec-Fetch-Site"
)

// Common header values.
const (
	AcceptAny  = "*/*"
	AcceptJSON = "application/json, text/plain, */*"
	AcceptHTML = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

	ContentTypeJSON = "application/json"
	ContentTypeForm = "application/x-www-form-urlencoded"

	// EncodingIdentity asks for the raw bytes. A transparently compressed
	// body would make the progress total disagree with what lands on disk.
	EncodingIdentity = "identity"
)

// Merge returns a copy of base with the entries of extra applied over it.
// Either may be nil.
func (h Header) Merge(extra Header) Header {
	out := make(Header, len(h)+len(extra))
	for k, v := range h {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
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
