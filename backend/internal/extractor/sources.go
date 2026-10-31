// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"net/url"
	"path"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Expanding a page of links into the files behind them.
//
// Index extractors answer "what is behind this whole page of links"
// rather than "what is behind this one link": the harvester (links.go),
// the bunkr album index (balbums.go), and the K2S video index (voyeurking.go).
// None hosts anything itself; each resolves what it finds through its own
// registry.
//
// The parts they share are here, because each was arrived at for a reason
// and reproducing them slightly differently is how the second copy gets one
// of them wrong.

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

// expansion is what expandSources made of a list of sources.
type expansion struct {
	files []File
	// used is how many sources the files came from.
	used int
	// partial counts sources that themselves reported an incomplete result.
	partial int
	// full reports that the file cap ended the expansion: sources after the
	// last one used were never resolved.
	full bool
	// cut and of are the files kept and held by the one source trimmed to
	// fit, when a single source alone held more than the cap. Zero
	// otherwise: every other source is taken whole or not at all.
	cut, of int
	// unrecognised counts links that had a supported form but whose page
	// no extractor recognised. They were never sources, so they count
	// neither as resolved nor as failed.
	unrecognised int
	// refusals and failed are why sources did not resolve: the reasons of
	// those refused, in order, and the first other failure, for a caller
	// whose sources all failed to say why (noneRead).
	refusals []string
	failed   error
}

// expandSources resolves the sources in order, several at a time, until
// the file cap is reached.
//
// The concurrency is the one a listing expanded page-by-page uses, and for
// the same reason rather than by coincidence: a thread's two hundred links
// are usually two hundred links to the same host, so this is a burst at one
// host however many hosts the page names.
//
// The cap is applied a source at a time. A source is taken whole or not at
// all, so the job never ends halfway through an album with nothing to say
// which one; and resolving stops once the cap is reached, rather than
// resolving five hundred albums and throwing more than half their files
// away. The one exception is a first source that alone holds more than the
// cap, which is cut rather than leaving the job empty.
//
// Results are collected by index rather than appended as they arrive, so the
// job lists its files in the order the page did however the requests
// interleave. A link that will not resolve is skipped rather than failing the
// job: every thread of any age has dead links in it, and the live ones are
// still worth having.
func expandSources(ctx context.Context, registry *Registry, sources []string, opts Options) expansion {
	// Each source was chosen for the form of its link, so a page that turns
	// out to be no extractor's is navigation and is dropped.
	return expandSourcesWith(ctx, sources, opts, func(ctx context.Context, link string) (*Result, error) {
		res, _, err := registry.follow(ctx, link, opts)
		return res, err
	})
}

// expandSourcesWith also lets a recognised platform read its own posts,
// without registering every page on that host as a supported source.
func expandSourcesWith(ctx context.Context, sources []string, opts Options, extract func(context.Context, string) (*Result, error)) expansion {
	return expandSourcesNamed(ctx, sources, opts, extract, sourceFolders)
}

// expandSourcesNamed is expandSourcesWith for a host whose sources were
// filed under names of its own before they came here: folders names the
// folder of each source resolved, in order, so that everything already
// downloaded under those names stays where it is.
func expandSourcesNamed(ctx context.Context, sources []string, opts Options,
	extract func(context.Context, string) (*Result, error), folders func([]sourceResult) []string) expansion {
	limit := opts.maxFiles()

	// A few rounds of slots at a time: enough that a slow source holds up
	// little, few enough that the cap overshoots by a handful of requests.
	batch := 4 * config.PageFetchConcurrency
	var (
		resolved []sourceResult
		e        expansion
		total    int
		// failures are the sources that did not resolve, less those no
		// extractor recognised, which were never sources.
		failures []outcome[string, *Result]
	)
walk:
	for start := 0; start < len(sources); start += batch {
		var batchResolved []sourceResult
		for _, out := range resolveOutcomes(ctx, sources[start:min(start+batch, len(sources))], false, extract) {
			switch {
			case errors.Is(out.err, errUnrecognised):
				e.unrecognised++
			case out.err != nil:
				failures = append(failures, out)
			case out.got != nil:
				batchResolved = append(batchResolved, sourceResult{link: out.item, res: out.got})
			}
		}
		for _, r := range batchResolved {
			n := len(r.res.Files)
			if total+n > limit {
				e.full = true
				if len(resolved) == 0 {
					e.cut, e.of = limit, n
					r.res.Files = r.res.Files[:limit]
					resolved = append(resolved, r)
				}
				break walk
			}
			resolved = append(resolved, r)
			total += n
		}
		if ctx.Err() != nil {
			break
		}
	}

	names := folders(resolved)
	for i, r := range resolved {
		if r.res.Note != "" {
			e.partial++
		}
		e.files = append(e.files, sourceFiles(r.res, names[i])...)
	}
	e.used = len(resolved)
	e.refusals, e.failed = sortOutcomes(failures)
	return e
}

// sourceFolders names each source's folder after its title.
//
// Titles are not unique: a search on an album index turned up 356 albums
// under 345 titles, and each pair sharing one was poured into the same
// folder. Two different files of the same name then sat side by side as
// "(2)" and "(3)", and nothing said which album either came from. So a
// title that repeats carries the source's own id; one that does not keeps
// its plain name, which is what every existing download under it is filed
// as. Compared case-blind, because the destination may be a filesystem that
// is.
func sourceFolders(resolved []sourceResult) []string {
	uses := make(map[string]int, len(resolved))
	for _, r := range resolved {
		uses[strings.ToLower(sourceFolder(r.res.Title))]++
	}
	names := make([]string, len(resolved))
	for i, r := range resolved {
		names[i] = sourceFolder(r.res.Title)
		if uses[strings.ToLower(names[i])] > 1 {
			names[i] = strings.TrimSpace(names[i] + " [" + sourceTag(r.link) + "]")
		}
	}
	return names
}

// sourceResult is one source resolved, kept whole until every source is in
// and folder names can be compared.
type sourceResult struct {
	link string
	res  *Result
}

// sourceTag identifies a source by its own URL, for a folder whose title
// another source shares: the last path segment, which for an album link is
// the album's id, or the host where the path says nothing.
func sourceTag(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return sourceFolder(link)
	}
	segs := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	if len(segs) > 0 {
		return sourceFolder(segs[len(segs)-1])
	}
	return u.Hostname()
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
func sourceFiles(res *Result, folder string) []File {
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
// otherwise is the manager's business, not an extractor's. The lookalikes a
// nested extraction's title arrives with (util.TitleName) go the same way, so
// that a folder keeps the name everything already downloaded under it has.
func sourceFolder(title string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', '\u29f8', '\u29f9':
			return '_'
		}
		return r
	}, strings.TrimSpace(title))
}

// partialNote admits what the caps left out of an expansion, for the job's
// Note.
//
// Silently returning the first thousand of three thousand files would be
// indistinguishable from the page having a thousand, so a capped job says
// so where its name is read. It says nothing otherwise — a dead link or two
// is a page's own business, not a cap's. found is how many sources the page
// or search had; capped reports that the sources walked stopped short of
// it. The noun is the caller's because what was truncated is its own
// vocabulary: a thread has links on it, a search has albums.
func partialNote(noun string, e expansion, found int, capped bool) string {
	if !capped && !e.full {
		return ""
	}
	return joinNotes(cappedNote(e.used, found, noun), cappedNote(e.cut, e.of, "files"))
}

// expansionNote is a walked listing's note once its entries are expanded:
// the walk's own, the file cap when it cut the expansion short, and how
// many of the listed entries resolved when some did not, in the caller's
// noun.
func expansionNote(walk string, e expansion, listed int, noun string) string {
	limit := ""
	if e.full {
		limit = fileLimitNote
	}
	return joinNotes(walk, limit, resolvedNote(e.used, listed, noun))
}

// opening is an expansion's extract for a host reading the pages its own
// listing links: each link parsed and handed to open.
func opening(open func(context.Context, *url.URL) (*Result, error)) func(context.Context, string) (*Result, error) {
	return func(ctx context.Context, link string) (*Result, error) {
		u, err := ParseURL(link)
		if err != nil {
			return nil, err
		}
		return open(ctx, u)
	}
}

// limitNote is fileLimitNote when the file cap ended the expansion.
func (e expansion) limitNote() string {
	if e.full {
		return fileLimitNote
	}
	return ""
}
