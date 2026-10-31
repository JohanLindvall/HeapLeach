// SPDX-License-Identifier: MIT

// Package extractor turns a page URL into the concrete list of files behind
// it. Each supported host gets one Extractor; unknown hosts fall through to
// a direct-link extractor.
package extractor

import (
	"context"
	"net/url"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Result is everything an extractor found behind one input URL.
type Result struct {
	// Title names the job and the destination folder.
	Title string
	// Note is what the job's name adds for whoever is looking at it — that
	// a listing was cut short by a cap or a rate limit, and how far. It is
	// shown beside the title and never becomes part of the folder: the
	// folder is the title's alone, so a job read again with a higher cap,
	// or once the rate limit has lifted, finds everything it already
	// downloaded where it left it. A note in the title itself moved the
	// whole job to a new folder the moment the note changed.
	Note string
	// Files are the resources to download, in display order.
	Files []File
}

// Label is the title with its note, for display.
func (r *Result) Label() string {
	if r.Note == "" {
		return r.Title
	}
	return r.Title + " (" + r.Note + ")"
}

// Extractor resolves one family of pages: it recognises them (Recogniser),
// says which for the inventory (Describer), and extracts them.
type Extractor interface {
	Recogniser
	Describer
	// Name is the short label shown in the UI.
	Name() string
	// Extract resolves the URL into downloadable files. opts may be zero.
	Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error)
}

// Recogniser is how an extractor claims a URL without naming its host.
//
// Nothing in either method may depend on the URL's host. The registry
// fetches the page first (probePage) and gives it to each extractor's
// Recognise in turn; the first to claim it extracts it. So an extractor is
// chosen by what the site serves — its name, its player, its bundle, its API
// answering — and a site that moves domain, or an install nobody listed,
// still reaches the right code.
type Recogniser interface {
	// Shape reports whether the URL has a form this extractor reads, by its
	// path, query and fragment. It is all the link scanner has to go on
	// before fetching, so it decides which links on a page are worth a
	// probe; a link of the right form that turns out to be someone else's
	// page is dropped after its probe. Report false for forms too common to
	// single out — the site root, a bare slug — rather than turn every
	// navigation link into a request.
	Shape(u *url.URL) bool
	// Recognise reports whether the probed page is one this extractor
	// reads. It may make a bounded request of its own (platformJSON) when
	// the page cannot say — a challenge stood in for it, say — but only once
	// the URL's form has made the answer likely: every unrecognised page is
	// offered to every extractor.
	Recognise(ctx context.Context, p *Page) bool
}

// Options carries per-request extras supplied by the caller.
type Options struct {
	// Password unlocks protected folders.
	Password string

	// limits is stamped by Registry.Extract from the configuration, so it
	// reaches every extractor — including one reached through another, an
	// album found by a search. Unexported, so only this package can set it:
	// a caller outside gets what the configuration says. Read through
	// maxSources and maxFiles.
	limits Limits
	// ancestors is local to one expansion path, so independent sources can
	// resolve concurrently without sharing a mutable visited set.
	ancestors []string
	// strict marks a source followed for the form of its link: a page that
	// no extractor recognises is navigation, and is reported as
	// errUnrecognised rather than handed to the fallback. It applies to that
	// one URL, never to what its extractor goes on to resolve.
	strict bool
}

// Limits bound what one submitted URL may expand to. Zero in either means
// the built-in default.
type Limits struct {
	// Sources is how many of a page's links are followed: the albums of an
	// index search, the links of a harvested thread.
	Sources int
	// Files is how many files one submitted URL may resolve to.
	Files int
}

// maxSources is how many of a page's links may be followed.
func (o Options) maxSources() int {
	if o.limits.Sources > 0 {
		return o.limits.Sources
	}
	return config.MaxExpandedSources
}

// maxFiles is how many files one submitted URL may resolve to.
func (o Options) maxFiles() int {
	if o.limits.Files > 0 {
		return o.limits.Files
	}
	return config.MaxListingFiles
}

// about is what an extractor says of itself to the registry and the
// inventory: its name, the forms of link worth a probe, and the
// inventory's description of what it recognises. An extractor embeds it
// rather than spelling out the three methods, so that its constructor
// reads as its row.
type about struct {
	name string
	// shape is the forms Shape reports; none when nil.
	shape      func(*url.URL) bool
	recognises string
}

func (a about) Name() string { return a.name }

// Recognises is the inventory's description.
func (a about) Recognises() string { return a.recognises }

// Shape reports the forms of link worth a probe.
func (a about) Shape(u *url.URL) bool { return a.shape != nil && a.shape(u) }

// eitherShape is a shape matching what any of shapes match.
func eitherShape(shapes ...func(*url.URL) bool) func(*url.URL) bool {
	return func(u *url.URL) bool {
		for _, shape := range shapes {
			if shape(u) {
				return true
			}
		}
		return false
	}
}

// linkFile is the file at link, named as the link names it, its length
// left to the response, fetched with headers.
func linkFile(link string, headers httpx.Header) File {
	return File{Name: util.NameFromURL(link), URL: link, Size: -1, Headers: headers}
}
