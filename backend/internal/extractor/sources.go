package extractor

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// Expanding a page of links into the files behind them.
//
// Two extractors here answer "what is behind this whole page of links"
// rather than "what is behind this one link": the harvester (links.go),
// pointed at a forum thread, and the bunkr album index (balbums.go), which
// is a search over somebody else's host. Neither hosts anything itself, and
// both resolve what they find through the registry they belong to.
//
// The parts they share are here, because each was arrived at for a reason
// and reproducing them slightly differently is how the second copy gets one
// of them wrong.

// maxExpandedSources bounds how many of a page's links are followed. Each
// one is a full extraction, several requests at some hosts, and a page
// carries more of them than one might guess — a tube's own front page
// measured just under two hundred, and a search over an index of six hundred
// thousand albums is unbounded in principle. So this is set where following
// the lot stops plausibly being what anybody asked for.
const maxExpandedSources = 500

// supportedSources keeps the candidates that reach a real extractor.
//
// Registry.Find never returns nil, so asking it whether a link is supported
// answers yes for every link on the page; Known is the question that has an
// answer worth acting on. The caller excludes itself on top of that: a page
// of links carries its own navigation, and an extractor that followed a link
// back to its own host would walk the site it is reading — a harvest that
// harvested, or an index whose "next page" became a second search.
//
// Self is recognised by name rather than by pointer, because the recursion
// is in the kind of extractor and not in the instance: a harvest reached
// through the prefix and one reached through a link are two different
// objects doing the same unbounded thing.
func supportedSources(registry *Registry, candidates []string, self Extractor) []string {
	out := make([]string, 0, len(candidates))
	for _, link := range candidates {
		u, err := ParseURL(link)
		if err != nil {
			continue
		}
		ex, known := registry.Known(u)
		if !known || ex.Name() == self.Name() {
			continue
		}
		out = append(out, link)
	}
	return out
}

// expandSources resolves every source, several at a time.
//
// The bound is the one a listing expanded page-by-page uses, and for the
// same reason rather than by coincidence: a thread's two hundred links are
// usually two hundred links to the same host, so this is a burst at one host
// however many hosts the page names.
//
// Results are collected by index rather than appended as they arrive, so the
// job lists its files in the order the page did however the requests
// interleave. A link that will not resolve is skipped rather than failing the
// job: every thread of any age has dead links in it, and the live ones are
// still worth having.
func expandSources(ctx context.Context, registry *Registry, sources []string, opts Options) []File {
	return FanOut(ctx, sources, func(ctx context.Context, link string) ([]File, error) {
		res, _, err := registry.Extract(ctx, link, opts)
		if err != nil {
			return nil, err
		}
		return sourceFiles(res), nil
	})
}

// sourceFiles takes one source's result into the collection.
//
// Each File is copied through untouched but for its directory — resolver,
// cipher, headers and size all as its own extractor left them — because those
// are what make the file downloadable at all, and a caller that rewrote them
// would only work on hosts serving plain links. It is the reason this
// composes: bunkr's twenty-minute signed links are still minted at transfer
// time, gofile's mirrors still rotate on each attempt, and mega's file key
// still travels with the file.
//
// Every source gets a folder, even one holding a single file. The manager
// takes the opposite view for a job, and is right to: one file does not need
// a directory. This is the case that inverts it, because the job root would
// otherwise be a heap of hundreds of files from unrelated sources with
// nothing but their names to say which came from where.
func sourceFiles(res *Result) []File {
	folder := sourceFolder(res.Title)
	files := make([]File, 0, len(res.Files))
	for _, f := range res.Files {
		f.Dir = path.Join(folder, f.Dir)
		files = append(files, f)
	}
	return files
}

// sourceFolder reduces a source's title to one directory component.
//
// SafeName, which sanitises these properly, lives in the download package and
// the dependency runs the other way, so this does only the part that has to
// happen before the name is handed over: a title carrying a separator would be
// split by the manager's own SafeRelPath, and "A Creator / An Album" would
// arrive as two nested folders rather than one. What the name may contain
// otherwise is the manager's business, not an extractor's.
func sourceFolder(title string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, strings.TrimSpace(title))
}

// partialTitle names the job, admitting anything the caps left out.
//
// An extractor has no logger and a Result carries nothing but a title and its
// files, so the title is the only place a partial answer can be declared — and
// it is a good one, being what names the job in the UI and the folder on disk,
// which is exactly where somebody comparing a page against what they got
// will look. Silently returning the first thousand of three thousand files
// would be indistinguishable from the page having a thousand. The noun is the
// caller's because what was truncated is its own vocabulary: a thread has
// links on it, a search has albums.
func partialTitle(title, noun string, sources, found, files, resolved int) string {
	var notes []string
	if sources < found {
		notes = append(notes, fmt.Sprintf("%d of %d %s", sources, found, noun))
	}
	if files < resolved {
		notes = append(notes, fmt.Sprintf("%d of %d files", files, resolved))
	}
	if len(notes) == 0 {
		return title
	}
	return title + " (" + strings.Join(notes, ", ") + ")"
}
