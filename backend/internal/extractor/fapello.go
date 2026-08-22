package extractor

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Fapello resolves a model's page into their media.
//
// Listing pages carry the model's own thumbnails alongside recommendations
// for other models, so entries are kept only when the path names the model
// being fetched. The thumbnails differ from the full-size file by a "_300px"
// suffix, which is simply dropped.
type Fapello struct {
	client *httpx.Client
}

const (
	fapelloRoot = "https://fapello.com"
	// fapelloMaxPages bounds the walk; paging past the end wraps back to
	// the first page rather than returning nothing, so the real stop
	// condition is a page that adds nothing new.
	fapelloMaxPages = 200
)

// fapelloThumbSuffix marks a listing thumbnail.
var fapelloThumbSuffix = regexp.MustCompile(`_\d+px(\.[A-Za-z0-9]+)$`)

// NewFapello builds the fapello extractor.
func NewFapello(client *httpx.Client) *Fapello { return &Fapello{client: client} }

func (f *Fapello) Name() string { return "fapello" }

func (f *Fapello) Match(u *url.URL) bool {
	return util.HostMatches(u.Host, "fapello.com")
}

// Extract walks a model's pages and collects everything of theirs.
func (f *Fapello) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) == 0 {
		return nil, fmt.Errorf("fapello: %s names no model", u.Redacted())
	}
	model := segs[0]

	// Keep whichever regional host was given: they serve the same content
	// and the site is happier being asked on the one it advertised.
	base := util.Origin(u)
	headers := httpx.Referer(base + "/")

	// Media lives on the shared content host and always names the model.
	ownMedia := regexp.MustCompile(
		`https?://[a-z0-9.-]*fapello\.com/content/[^"'\\ <>]*/` +
			regexp.QuoteMeta(model) + `/[^"'\\ <>]+`)

	seen := make(map[string]bool)
	var files []File

	for page := 1; page <= fapelloMaxPages; page++ {
		target := base + "/" + url.PathEscape(model) + "/"
		if page > 1 {
			target += "page-" + strconv.Itoa(page) + "/"
		}

		doc, err := f.client.GetString(ctx, target, headers)
		if err != nil {
			if page == 1 {
				return nil, fmt.Errorf("fapello: fetch %s: %w", target, err)
			}
			break
		}

		var added int
		for _, link := range ownMedia.FindAllString(doc, -1) {
			full := fapelloFullSize(link)
			if seen[full] {
				continue
			}
			seen[full] = true
			added++
			files = append(files, File{
				Name:    util.NameFromURL(full),
				URL:     full,
				Size:    -1,
				Headers: headers,
			})
		}
		// Paging past the last page wraps to the first, so a page that
		// contributes nothing means the walk is over.
		if added == 0 {
			break
		}
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("fapello: found no media for %q", model)
	}
	return &Result{Title: model, Files: files}, nil
}

// fapelloFullSize turns a listing thumbnail into the full-size file.
func fapelloFullSize(link string) string {
	if strings.Contains(link, "px.") {
		return fapelloThumbSuffix.ReplaceAllString(link, "$1")
	}
	return link
}
