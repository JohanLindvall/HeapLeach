// Package util holds small, dependency-free helpers shared by the other
// packages. Anything here must be generic enough that no single package owns
// it; host- or protocol-specific logic belongs with its own package.
package util

import (
	"context"
	"encoding/base64"
	"errors"
	"math/rand/v2"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// FirstNonEmpty returns the first non-empty string, or "" when all are empty.
func FirstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Dedupe returns values with empties and duplicates removed, order preserved.
func Dedupe(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// Remove returns values with the first occurrence of v removed. The input
// slice is not modified.
func Remove(values []string, v string) []string {
	for i, x := range values {
		if x == v {
			return append(values[:i:i], values[i+1:]...)
		}
	}
	return values
}

// SortBy sorts values in place using a less function.
func SortBy(values []string, less func(a, b string) bool) {
	sort.Slice(values, func(i, j int) bool { return less(values[i], values[j]) })
}

// Truncate collapses whitespace and caps a string at n characters, for
// embedding a remote response in an error message.
func Truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// NameFromURL derives a filename from a URL's path, ignoring the query
// string and undoing percent-encoding. It returns "" when there is no
// usable path segment.
func NameFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	base := path.Base(u.Path)
	if base == "." || base == "/" || base == "" {
		return ""
	}
	if decoded, err := url.PathUnescape(base); err == nil {
		base = decoded
	}
	return strings.TrimSpace(base)
}

// Origin renders scheme://host for a URL.
func Origin(u *url.URL) string { return u.Scheme + "://" + u.Host }

// HostMatches reports whether host is domain, or a subdomain of it. Any port
// and trailing dot are ignored.
func HostMatches(host, domain string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// PathSegments splits a URL path into its non-empty segments.
func PathSegments(u *url.URL) []string {
	var out []string
	for _, s := range strings.Split(u.Path, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// TrimTitleSuffix drops a trailing site name from a document title.
//
// Separators are tried in priority order and only the first one present is
// used, cutting at its first occurrence. That handles both conventions these
// hosts use without the two interfering: "My - Clip.mp4 | Bunkr" keeps its
// dash because " | " matches first, while "Album - Photos - EroMe", which
// has no pipe, is cut at the leading dash.
func TrimTitleSuffix(title string, separators ...string) string {
	title = strings.TrimSpace(title)
	for _, sep := range separators {
		if i := strings.Index(title, sep); i > 0 {
			return strings.TrimSpace(title[:i])
		}
	}
	return title
}

// Backoff returns an exponentially growing delay with +/-25% jitter, capped
// at max. Jitter keeps a burst of retries from synchronising on one host.
func Backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	grown := base * (1 << min(attempt, 6))
	jittered := time.Duration(float64(grown) * (0.75 + rand.Float64()/2))
	if jittered > max {
		return max
	}
	return jittered
}

// SleepCtx waits for d, returning early with the context's error if it is
// cancelled first. A non-positive duration returns immediately.
func SleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// base64Alphabet maps the standard alphabet onto the URL-safe one, so a
// value written either way decodes.
var base64Alphabet = strings.NewReplacer("+", "-", "/", "_")

// DecodeBase64 decodes base64 in whichever of its four spellings arrived:
// standard or URL-safe, padded or not. Remote pages are not consistent about
// which they use, and the difference never carries meaning.
func DecodeBase64(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	if s == "" {
		return nil, errors.New("empty base64 value")
	}
	return base64.RawURLEncoding.DecodeString(base64Alphabet.Replace(s))
}
