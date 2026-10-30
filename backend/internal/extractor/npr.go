// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/html"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// NPR resolves npr.org pages into the audio they publish.
//
// The American public broadcaster is the odd one out among the broadcasters
// here, because there is no player API to ask. There is an API, and it is the
// trap rather than the answer: api.npr.org wants a key and refuses an
// anonymous caller outright, which is why yt-dlp's own NPR extractor fails on
// every link with a bare 403. What is open is the page. Every piece of audio
// NPR publishes carries its own metadata in the markup, as a JSON document in
// a data-audio attribute, and reading those is the whole of this extractor.
// One page yields one segment or forty depending on what it is — a story, a
// programme's rundown, a podcast's episode list — and all three are the same
// parse, which is what makes the programme and podcast pages a bulk download
// rather than a file.
//
// Three things about those blocks decide the rest.
//
// The first is that a block states its own length, and whether that can be
// believed depends on which of NPR's two delivery paths it names. A segment
// on NPR's own on-demand store is a static file and the figure is exact to
// the byte — worth having, since an exact Size is what lets the downloader
// skip an episode already on disk without opening a connection. A podcast
// episode goes through a chain of analytics redirectors instead and is
// stitched together per request with advertising, so what arrives is
// several megabytes longer than the page claims: 36,461,592 advertised
// against 39,777,666 delivered, and not one episode of a page of them
// matched. Carrying that as an exact Size is precisely the lie File.Size
// forbids, so the figure is taken from the store's own links and nowhere
// else; the redirected ones wait for the response headers, which is the
// check that works anyway.
//
// The second is that a block's own fields need the same suspicion.
// "available" being true does not mean there is anything to fetch: NPR+
// subscriber episodes are listed exactly like the rest, differing only in an
// empty audioUrl and the subscription named in podcastEpisodeDerivedPlusType.
// That is the refusal worth reporting in NPR's own terms — a podcast page
// whose bonus episodes are all withheld should say so rather than report
// itself as empty.
//
// The third is what does not need doing. A podcast link's redirect chain is
// four hops and the far end is signed per request, but the entry URL is
// stable and unsigned, so every connection and every retry re-follows the
// chain and signs itself. There is no File.Resolve here for the same reason
// the part file goes on naming the same transfer between runs: nothing about
// the URL the page published expires.
type NPR struct {
	client *httpx.Client
}

const (
	// nprAudioAttr holds one piece of audio's metadata, as JSON in an
	// attribute. It is read with the HTML tokeniser rather than matched out
	// of the page text, and the ampersands are the reason: NPR writes its
	// query strings unencoded, and the rule that keeps "&sect=" from becoming
	// a section sign lives in the tokeniser and not in any table of entities.
	// The apostrophes that would otherwise close the attribute are escaped a
	// level lower down, in the JSON.
	nprAudioAttr = "data-audio"

	// nprStore is NPR's own on-demand store: the one host whose links state a
	// length that survives being checked.
	nprStore = "ondemand.npr.org"

	// nprSizeParam is where that length is written.
	nprSizeParam = "size"

	// nprSiteSuffix ends every document title. It is trimmed as one fixed
	// string rather than cut at a separator, since NPR writes headlines that
	// contain a colon of their own.
	nprSiteSuffix = " : NPR"

	// nprMediaType is what NPR publishes throughout, and is what names the
	// saved file when a redirector's URL ends in an id instead of a filename.
	nprMediaType = "audio/mpeg"
)

// NewNPR builds the NPR extractor.
func NewNPR(client *httpx.Client) *NPR { return &NPR{client: client} }

func (n *NPR) Name() string { return "npr" }

// nprShape is a story (/<yyyy>/<mm>/<dd>/<id>/<slug>), a programme
// (/programs/<slug>/) or a podcast (/podcasts/<id>/<slug>).
var nprShape = pathShape(`^/[0-9]{4}/[0-9]{2}/[0-9]{2}/(?:[0-9]{6,}|nx-s[0-9]+-[0-9]+)/[^/]+`, `^/programs/[^/]+`, `^/podcasts/[0-9]+/[^/]+`)

// Recognises is the inventory's description.
func (n *NPR) Recognises() string {
	return "NPR pages carrying audio players (`data-audio`): stories, programmes, podcasts and sections"
}

// Shape reports a story, programme or podcast link.
func (n *NPR) Shape(u *url.URL) bool { return nprShape(u) }

// Recognise takes an NPR page carrying audio, in the player blocks
// (data-audio) this lists: any page of the site that has them, a section
// front as much as a story.
func (n *NPR) Recognise(_ context.Context, p *Page) bool {
	return len(p.Segments()) > 0 && p.Has(nprAudioAttr+"=") && p.Named("NPR")
}

// Extract reads a page and lists every piece of audio on it.
func (n *NPR) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	// No headers: the pages need none, and neither does the audio they name.
	doc, _, err := fetchDocument(ctx, n.client, u, nil)
	if err != nil {
		return nil, fmt.Errorf("npr: fetch %s: %w", u.Redacted(), err)
	}
	return nprResult(doc, u)
}

// nprResult turns a fetched page into a job. Kept apart from the fetch so a
// fixture can stand in for the page, since every decision this host needs is
// made here.
func nprResult(doc string, u *url.URL) (*Result, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("npr: %s: %w", u.Redacted(), err)
	}

	listed := nprAudioFrom(root)
	if len(listed) == 0 {
		return nil, fmt.Errorf("npr: no audio on %s — the page embeds none, which is how a text "+
			"or video story looks; the pages that carry audio are a story, a programme "+
			"(/programs/<slug>/) and a podcast (/podcasts/<id>/<slug>)", u.Redacted())
	}

	var refused []nprRefusal
	playable := make([]nprAudio, 0, len(listed))
	for _, entry := range listed {
		if reason := entry.refusal(); reason != nprPlayable {
			refused = append(refused, reason)
			continue
		}
		playable = append(playable, entry)
	}
	reasons := nprReasons(refused)
	if len(playable) == 0 {
		return nil, fmt.Errorf("npr: none of the %d pieces of audio on %s can be fetched: %s",
			len(listed), u.Redacted(), nprTally(reasons))
	}

	title := cmp.Or(nprPageTitle(root), strings.Trim(u.Path, "/"))
	return &Result{Title: title, Files: nprFiles(playable), Note: refusedNote(reasons)}, nil
}

// nprAudioFrom pulls every audio block out of a parsed page, in document
// order and with the repeats dropped.
//
// A page renders the same player module more than once — the segment featured
// at the head of a rundown appears again in the list below it, with identical
// metadata both times — so the list is folded on the uid NPR gives each piece
// of audio, and on the URL as well, since a block with no uid must still not
// be queued twice.
func nprAudioFrom(root *html.Node) []nprAudio {
	var out []nprAudio
	seen := make(map[string]bool)
	for _, n := range findAll(root, func(n *html.Node) bool { return attr(n, nprAudioAttr) != "" }) {
		var entry nprAudio
		if err := json.Unmarshal([]byte(attr(n, nprAudioAttr)), &entry); err != nil {
			continue
		}
		if nprSeen(seen, entry.UID) || nprSeen(seen, entry.AudioURL) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// nprSeen records a key and reports whether it had been recorded already. An
// empty key is no identity at all, so it never matches and never claims one.
func nprSeen(seen map[string]bool, key string) bool {
	if key == "" {
		return false
	}
	if seen[key] {
		return true
	}
	seen[key] = true
	return false
}

// nprFiles turns the playable blocks into the download list.
func nprFiles(entries []nprAudio) []File {
	// Only file by programme when there is more than one to tell apart. A
	// programme's own page is all one strand and a folder would say nothing;
	// a section or front page is a handful of them at once, and reads as a
	// heap without.
	nest := nestByLabel(entries, nprAudio.program)

	used := make(map[string]int, len(entries))
	files := make([]File, 0, len(entries))
	for _, entry := range entries {
		file := File{
			// Named by the syndication extractor's own rules, which is not a
			// shortcut taken for brevity: an NPR page's audio is the same
			// podcast MP3 that page's feed encloses, wrapped by the same
			// redirectors, so what to do with a slash in a title, which end of
			// a wrapped URL carries the publisher's own filename, and how two
			// segments sharing a title avoid one file all have the same
			// answers here as they do there.
			Name: feedFileName(entry.Title, entry.AudioURL, nprMediaType, used),
			URL:  entry.AudioURL,
			Size: nprSize(entry.AudioURL),
			// No headers: this audio is published to be fetched by any
			// podcast client in the world, and the redirectors in front of it
			// read nothing but the user agent, which the client already sends.
		}
		if nest {
			file.Dir = sourceFolder(entry.program())
		}
		files = append(files, file)
	}
	return files
}

// nprSize reads the byte count off an audio URL, and only where it can be
// believed — see the delivery paths above. On the on-demand store it is the
// file's own length; anywhere else it is what the episode measured before
// advertising was stitched into it, which is a different number from the one
// that will arrive.
func nprSize(rawURL string) int64 {
	u, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(u.Hostname(), nprStore) {
		return -1
	}
	return parsedSize(u.Query().Get(nprSizeParam))
}

// nprPageTitle names the job after the page. The social metadata carries the
// headline already stripped of the site's own suffix, and the document title
// is the fallback that still carries it.
func nprPageTitle(root *html.Node) string {
	if title := metaContent(root, "og:title"); title != "" {
		return title
	}
	return strings.TrimSpace(strings.TrimSuffix(firstText(root, atomTitle), nprSiteSuffix))
}

// nprAudio is one piece of audio, as the page describes it. What is left
// unread is the story it belongs to, its duration, and a good deal of
// advertising bookkeeping.
type nprAudio struct {
	// UID identifies the audio, and is what a page's repeated blocks are
	// folded together on.
	UID string `json:"uid"`
	// Available is NPR's own word for whether the audio may be played. It is
	// necessary and nothing like sufficient — see refusal.
	Available bool `json:"available"`
	// Title is what NPR calls the audio, which for a news segment is the desk
	// slug rather than the story's headline.
	Title string `json:"title"`
	// AudioURL is the file, either on NPR's store or at the head of a
	// podcast's redirect chain.
	AudioURL string `json:"audioUrl"`
	// Program is the strand it was broadcast in, and is what a page spanning
	// several of them is filed by.
	Program string `json:"program"`
	// IsStreamAudio marks NPR's continuous live stream rather than a
	// recording. It has no length and no end, so it is not a download.
	IsStreamAudio bool `json:"isStreamAudioType"`
	// PlusType names the subscription tier an episode was classified into,
	// and is what tells a bonus episode withheld from non-subscribers apart
	// from one that is merely missing.
	PlusType string `json:"podcastEpisodeDerivedPlusType"`
}

// program is the strand this belongs to, tidied for use as a folder name.
func (a nprAudio) program() string { return strings.TrimSpace(a.Program) }

// nprRefusal is why a listed block cannot be fetched, or nprPlayable when it
// can be. The refusals are in the order a page's are reported in.
type nprRefusal int

const (
	nprPlayable nprRefusal = iota
	nprSubscriberOnly
	nprUnavailable
	nprLiveStream
	nprNoAudio
)

// String reads a refusal back in NPR's terms rather than in ours.
func (r nprRefusal) String() string {
	switch r {
	case nprSubscriberOnly:
		return "behind an NPR+ subscription"
	case nprUnavailable:
		return "marked unavailable"
	case nprLiveStream:
		return "listed as a live stream, which has no end to download to"
	}
	return "listed with no audio at all"
}

// refusal reports whether this block is worth queueing, and why not when it
// is not.
//
// The order is the point. A block can answer to several of these at once, and
// the subscription is consulted only once the audio is known to be missing:
// NPR+ also classifies episodes that are perfectly fetchable — the
// sponsor-free cut of an ordinary one — so a non-empty tier means "withheld"
// only where there was nothing to withhold it from.
func (a nprAudio) refusal() nprRefusal {
	switch {
	case !a.Available:
		return nprUnavailable
	case a.IsStreamAudio:
		return nprLiveStream
	case a.AudioURL != "":
		return nprPlayable
	case a.PlusType != "":
		return nprSubscriberOnly
	}
	return nprNoAudio
}

// nprReasons are why a page's audio was passed over, in the order a page's
// refusals are reported in, so the note and the error read alike.
func nprReasons(refused []nprRefusal) []string {
	slices.Sort(refused)
	reasons := make([]string, len(refused))
	for i, r := range refused {
		reasons[i] = r.String()
	}
	return reasons
}

// nprTally reads back as the tail of a sentence about a page that yields
// nothing, saying which of NPR's reasons applied instead of reporting the
// page as empty.
func nprTally(reasons []string) string {
	var parts []string
	for _, c := range tally(reasons) {
		parts = append(parts, fmt.Sprintf("%d %s", c.n, c.reason))
	}
	if len(parts) == 0 {
		return "the page lists them but names nothing to fetch"
	}
	return strings.Join(parts, ", ")
}
