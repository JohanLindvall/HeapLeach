// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
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
//
// from is the page the candidates were found on. Ordinary discovery uses
// Registry.Follows; the explicit links: harvester also accepts established
// item routes on known installs (Registry.harvests).
func supportedSources(registry *Registry, candidates []string, self Extractor, from *url.URL) []string {
	follows := registry.Follows
	if self.Name() == linksScheme {
		follows = registry.harvests
	}
	out := make([]string, 0, len(candidates))
	for _, link := range candidates {
		u, err := ParseURL(link)
		if err != nil {
			continue
		}
		ex, known := follows(u, from)
		if !known || ex.Name() == self.Name() {
			continue
		}
		out = append(out, link)
	}
	return out
}

// expansion is what expandSources made of a list of sources.
type expansion struct {
	// outcomeSummary is the account of every source the expansion
	// attempted, batch by batch: those that resolved, held nothing, were
	// refused, failed or turned out to be no extractor's page (unrecognised,
	// links with a supported form that were never sources, so neither
	// resolved nor failed). A source resolved but left out by the file cap
	// counts as resolved: it is not a failure.
	outcomeSummary
	files []File
	// used is how many sources the files came from: those the cap
	// admitted, counted apart from the batch work done past it.
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
	// out to be no extractor's holds no file of the job's: it is left out,
	// and counted as unrecognised in the note (expansion.note).
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
	e := sourceExpansion{limit: opts.maxFiles()}
	e.add(ctx, sources, extract)
	return e.result(folders)
}

// sourceExpansion admits sources as listing pages arrive, sharing one file
// budget and one whole-source boundary across them. Results stay together
// until result assigns folders, since titles can repeat on different pages.
type sourceExpansion struct {
	limit, total int
	listed       int
	resolved     []sourceResult
	outcomes     []outcome[string, *Result]
	full         bool
	cut, of      int
}

// done also covers a whole source that cannot fit the remaining budget.
// Exact exhaustion alone need not mean anything was left out (full).
func (e *sourceExpansion) done() bool { return e.full || e.total >= e.limit }

func (e *sourceExpansion) add(ctx context.Context, sources []string, extract func(context.Context, string) (*Result, error)) {
	offset := e.listed
	e.listed += len(sources)
	if len(sources) == 0 {
		return
	}
	if e.done() {
		e.full = true
		return
	}

	// A few rounds of slots at a time (config.SourceBatchRounds).
	batch := config.SourceBatchRounds * config.PageFetchConcurrency
	for start := 0; start < len(sources); start += batch {
		var batchResolved []sourceResult
		for _, out := range resolveOutcomes(ctx, sources[start:min(start+batch, len(sources))], false, extract) {
			out.index += offset + start
			e.outcomes = append(e.outcomes, out)
			// Empty successes are outcomes, but cannot consume admission
			// or the first nonempty source's permission to be trimmed.
			if out.err == nil && out.got != nil && len(out.got.Files) > 0 {
				batchResolved = append(batchResolved, sourceResult{link: out.item, res: out.got})
			}
		}
		for i, r := range batchResolved {
			n := len(r.res.Files)
			if e.total+n > e.limit {
				e.full = true
				if len(e.resolved) == 0 {
					e.cut, e.of = e.limit, n
					trimmed := *r.res
					trimmed.Files = trimmed.Files[:e.limit]
					r.res = &trimmed
					e.resolved = append(e.resolved, r)
					e.total = e.limit
				}
				return
			}
			e.resolved = append(e.resolved, r)
			e.total += n
			if e.total == e.limit {
				// Full to the file: no later source can be taken whole,
				// so none is resolved only to be left out.
				e.full = i < len(batchResolved)-1 || start+batch < len(sources)
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (s *sourceExpansion) result(folders func([]sourceResult) []string) expansion {
	e := expansion{used: len(s.resolved), full: s.full, cut: s.cut, of: s.of}
	names := folders(s.resolved)
	for i, r := range s.resolved {
		if r.res.Note != "" {
			e.partial++
		}
		e.files = append(e.files, sourceFiles(r.res, names[i])...)
	}
	e.outcomeSummary = summarise(s.listed, s.outcomes, func(res *Result) bool { return res != nil && len(res.Files) > 0 })
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

// sourceWords is how a collection words its expansion's note: what its
// sources are called, and which of the shared accounts it gives.
type sourceWords struct {
	// noun is the sources, in the collection's words: "albums", "links".
	noun string
	// found is how many sources the page or search offered, for a
	// collection that counts its caps against them (partialNote) rather
	// than naming the file limit; zero for one that names it.
	found int
	// counted says what was left out as how many sources resolved — "3 of
	// 5 links resolved" — rather than per reason, where no cap already
	// counted the sources against what was found.
	counted bool
	// own says the sources are pages of the collection's own, so that one
	// no extractor recognised is a dead page, counted as not resolved,
	// rather than navigation that only looked like a source.
	own bool
	// children is the words for the sources whose own results reported
	// something left out: "posts have incomplete downloads". Empty, they
	// read "N of M <noun> incomplete".
	children string
}

// note is a collection's note once its sources are expanded, the one place
// its parts are put together, in order: the listing's own (walk); what the
// caps left out; the sources left out, per reason (outcomeSummary.note) or
// as a count of those resolved (sourceWords.counted); and how many of the
// sources taken reported something left out of their own results. A link
// no extractor recognised counts as not resolved and is named as
// unrecognised, so that a site change breaking a recogniser for some of a
// page's links shows in the note rather than leaving a job that looks
// complete. Where the sources are the collection's own pages
// (sourceWords.own) such a page is a dead one, counted as not resolved
// alone.
func (e expansion) note(walk string, w sourceWords) string {
	caps := e.limitNote()
	if w.found > 0 {
		caps = partialNote(w.noun, e, w.found, e.listed < w.found)
	}
	omitted := e.outcomeSummary.note()
	if w.counted && (w.found == 0 || caps == "") {
		omitted = resolvedNote(e.used, e.listed, w.noun)
	}
	if !w.own {
		omitted = joinNotes(omitted, countNote(e.unrecognised, "unrecognised"))
	}
	return joinNotes(walk, caps, omitted, e.childNote(w))
}

// resolvedNote says how many of the sources resolved, in the caller's
// noun, and how many led to a page no extractor recognised, or nothing when
// every source resolved.
func (e expansion) resolvedNote(noun string) string {
	return joinNotes(resolvedNote(e.used, e.listed, noun), countNote(e.unrecognised, "unrecognised"))
}

// childNote counts the sources taken whose own results reported something
// left out, in the collection's words (sourceWords.children), or nothing
// when none did.
func (e expansion) childNote(w sourceWords) string {
	if w.children != "" {
		return countNote(e.partial, w.children)
	}
	if e.partial == 0 {
		return ""
	}
	return fmt.Sprintf("%d of %d %s incomplete", e.partial, e.used, w.noun)
}

// unresolved is how an expansion of the sources on where finishes when it
// is no success: the caller's cancellation, or noneRead's error when no
// source yielded files (outcomeSummary.noneResolved); nil otherwise.
func (e expansion) unresolved(ctx context.Context, site, noun, where string) error {
	return e.outcomeSummary.noneResolved(ctx, e.files, site, noun, where)
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
