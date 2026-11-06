// SPDX-License-Identifier: MIT

// Package util holds small, dependency-free helpers shared by the other
// packages. Anything here must be generic enough that no single package owns
// it; host- or protocol-specific logic belongs with its own package.
package util

import (
	"context"
	"encoding/base64"
	"errors"
	"html"
	"math/rand/v2"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

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

// Truncate collapses whitespace and caps a string at n bytes, for embedding
// a remote response in an error message. The cut lands on a rune boundary,
// so a multi-byte character is dropped whole rather than split into the
// replacement-character noise an invalid tail renders as.
func Truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// NameFromURL derives a filename from a URL: the name the link itself
// declares (DeclaredName) when it carries one, otherwise its path's last
// segment with percent-encoding undone. It returns "" when there is neither.
func NameFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if name := declaredName(u); name != "" {
		return name
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

// declaredNameParams are query parameters in which a link names the file it
// serves: a storage key's path is often an opaque id while the query says
// what the bytes are called, as the host's own download button would.
// `filename` is the plain form; the others carry a whole Content-Disposition
// value, as S3-compatible signed links (`response-content-disposition`) and
// Azure SAS links (`rscd`) do.
var declaredNameParams = []struct {
	key         string
	disposition bool
}{
	{"filename", false},
	{"response-content-disposition", true},
	{"rscd", true},
}

// DeclaredName is the filename a URL's query declares for the file it
// serves, or "" when it declares none. Such a name is the host's own name
// for the bytes, the same one its Content-Disposition would give, and so
// outranks anything a page title or an opaque path segment suggests.
func DeclaredName(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return declaredName(u)
}

func declaredName(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	q := u.Query()
	for _, p := range declaredNameParams {
		v := strings.TrimSpace(q.Get(p.key))
		if v == "" {
			continue
		}
		if p.disposition {
			v = DispositionName(v)
		}
		// A name, not a path: anything with a separator in it says where,
		// not what, and is no name to save under.
		if v = strings.TrimSpace(v); v != "" && v != "." && v != ".." && !strings.ContainsAny(v, `/\`) {
			return v
		}
	}
	return ""
}

// DispositionName is the filename a Content-Disposition value names, its
// RFC 5987 form decoded, or "" when it names none. Some servers
// percent-encode the plain form without saying so; that is undone too.
func DispositionName(v string) string {
	_, params, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	name := params["filename"]
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	return strings.TrimSpace(name)
}

// Stem is a name or path with its extension (path.Ext) removed; any
// directory is kept.
func Stem(name string) string {
	return strings.TrimSuffix(name, path.Ext(name))
}

// WithExt is name with its extension, if any, replaced by ext, which carries
// its own dot.
func WithExt(name, ext string) string {
	return Stem(name) + ext
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
	for s := range strings.SplitSeq(u.Path, "/") {
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

// ExecutableDir is the directory holding the running binary, symlinks
// resolved, where its helpers and data are installed beside it.
func ExecutableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
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

// TitleName keeps a title whole when it becomes a file or folder name. A
// slash in a title is not a path — "A Series (1/7)", "AC/DC" — but the
// downloader's SafeName takes everything after the last one, as it must for
// a name a server hands over, and "A Series (1/7)" would be saved as "7)".
// So each separator becomes its lookalike, the one yt-dlp writes too.
func TitleName(s string) string {
	return strings.NewReplacer("/", "\u29f8", "\\", "\u29f9").Replace(s)
}

// Unescape decodes HTML character references left in text that should
// already be plain. Some sites escape a title twice, so one parse leaves
// "Rafian&#39;s" or "this &amp; that" behind, and names written from it put
// the entity on disk. Repeated until the text stops changing, because
// "&amp;#39;" needs two passes; bounded, so a pathological string cannot
// spin. Text without an ampersand is returned as it is.
func Unescape(s string) string {
	for range 3 {
		next := html.UnescapeString(s)
		if next == s {
			break
		}
		s = next
	}
	return s
}
