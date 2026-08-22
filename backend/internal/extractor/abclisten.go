package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// ABCListen resolves abc.net.au/listen, the Australian broadcaster's radio
// and podcast service.
//
// Alone among the broadcasters here it needs no stream machinery at all.
// There is no player API to call, no adaptive manifest to pick a variant out
// of and nothing to decrypt: an episode page names its media as a plain MP3,
// AAC or MP4 on a CDN that answers a ranged request, and publishes the byte
// count to go with it. So this takes the ordinary transfer path — several
// connections, resume across runs, and the skip that costs no request at all
// — where svtplay and nrk both end up concatenating HLS parts.
//
// The route in is the page itself. Both API bases the site's own code names
// are shut to anonymous callers, terminus answering 401 and nucleus 403, and
// neither is needed: the JSON island every page carries, the
// <script id="__NEXT_DATA__"> its framework renders from, holds the whole
// document including the renditions.
//
// Three things about those renditions decided the code below, and each of
// them fails quietly rather than loudly.
//
// A rendition's MIME type is not to be believed. An AAC episode lists its
// file twice, once as audio/aac and once as application/vnd.apple.mpegurl —
// the HLS type — with the same .aac URL under both. So the extension comes
// from the URL, which the two entries agree on, and the twin is harmless
// instead of producing a file called .m3u8 that no player will open.
//
// The published length is exact only from the current media core. Everything
// it serves matched Content-Length to the byte, which is what makes the
// pre-connect skip work; the older library behind it publishes a figure it
// worked out instead, and the type it writes is the only tell. See
// abcListenRendition.measured.
//
// And an episode title is not unique within its programme, which is why
// abcListenName exists.
//
// What the ABC withholds, it withholds in two ways, and neither is a field
// that says "geo". A programme published through its app alone is flagged —
// abcListenAppOnly — and carries no media and no listing to work from. A
// recording licensed for Australia alone is flagged nowhere at all: it is
// offered like any other and simply sits in a different bucket on the CDN,
// which answers everyone else with 403. abcListenGeoBucket recognises that
// bucket and geoCheck turns the refusal into a sentence. Most of the
// catalogue is neither: pages and media alike answered a European address
// with no token, cookie or referer.
type ABCListen struct {
	client *httpx.Client
}

const (
	abcListenRoot = "https://www.abc.net.au"

	// abcListenSection is the one part of abc.net.au this claims. The domain
	// carries the whole broadcaster — the news site, iview, the education
	// pages — and matching it wholesale would promise far more than this
	// resolves.
	abcListenSection = "listen"

	// abcListenIsland is the id of the JSON document the page is rendered
	// from.
	abcListenIsland = "__NEXT_DATA__"

	// abcListenEpisodesPath lists a programme's episodes past the handful the
	// page itself renders. What follows it is routing decoration the server
	// does not read — the collection id is what selects, and a wrong path
	// with the right id answers correctly — so the programme's own path is
	// appended unchanged rather than reconstructed. It is joined to the
	// origin the programme was asked for rather than to abcListenRoot, so a
	// listing stays on whichever spelling of the host the caller used.
	abcListenEpisodesPath = "/core-next/api/episodesWithSegments"

	// abcListenPageSize is how many episodes one listing request returns.
	// Fifty is the server's own ceiling: ask for a hundred and it answers
	// with fifty and says so.
	abcListenPageSize = 50

	// abcListenMaxEpisodes bounds how many episodes one programme URL expands
	// to. The listing stops at 250 of its own accord today; this is the
	// backstop for the day it does not.
	abcListenMaxEpisodes = 500

	// abcListenTitleSuffix is what the site appends to every page title.
	abcListenTitleSuffix = " - ABC listen"

	// abcListenGeoBucketPrefix names the CDN path the ABC serves inside
	// Australia alone. See abcListenGeoBucket.
	abcListenGeoBucketPrefix = "geo-"
)

// NewABCListen builds the ABC listen extractor.
func NewABCListen(client *httpx.Client) *ABCListen { return &ABCListen{client: client} }

func (a *ABCListen) Name() string { return "abclisten" }

// Match takes abc.net.au under /listen and nothing else on the domain. iview
// is a subdomain of it and the news site shares this very page framework, but
// neither carries the document this reads.
func (a *ABCListen) Match(u *url.URL) bool {
	if !util.HostMatches(u.Host, "abc.net.au") {
		return false
	}
	segs := util.PathSegments(u)
	return len(segs) > 0 && segs[0] == abcListenSection
}

// Extract handles one episode or a whole programme. Which of the two a URL is
// comes from the page rather than from its shape: an episode page carries a
// document, a programme page carries a listing, and no page carries both.
func (a *ABCListen) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	page, err := a.page(ctx, u.String())
	if err != nil {
		return nil, err
	}
	props := page.props

	// Asked before anything is looked for, because a restricted programme
	// carries no media and no listing either: taken in the other order this
	// would report an empty page, which is true and useless.
	if props.restricted() {
		return nil, abcListenAppOnly(u.Redacted())
	}

	switch {
	case props.Data != nil:
		return a.episode(u, props.Data.DocumentProps)
	case props.Collection != nil && props.Collection.ID != "":
		return a.programme(ctx, u, page)
	}
	return nil, fmt.Errorf("abclisten: %s is neither an episode nor a programme listing", u.Redacted())
}

// episode resolves one document into a job of its own.
func (a *ABCListen) episode(u *url.URL, doc abcListenDoc) (*Result, error) {
	file, ok := a.file(doc)
	if !ok {
		return nil, fmt.Errorf("abclisten: %s carries no audio or video "+
			"(a presenter or topic page is a listing, not an episode)", u.Redacted())
	}

	// A lone file lands in the download directory with no folder to say what
	// it belongs to, so the name carries the programme as well: a daily
	// bulletin's own title is the programme's name and nothing more, which is
	// also why saying it twice is worth avoiding.
	title := strings.TrimSpace(doc.Title)
	switch programme := doc.programme(); {
	case title == "":
		title = programme
	case programme != "" && !strings.EqualFold(programme, title):
		title = programme + " - " + title
	}

	file.Name = abcListenName(doc.ID, title) + file.Name // Name holds only the extension here
	return &Result{Title: util.FirstNonEmpty(title, doc.ID), Files: []File{file}}, nil
}

// programme expands a programme into one file per episode.
//
// Every episode has to be opened. The listing names each one and links to it
// but keeps the media on the episode's own page, and carries nothing that
// stands in for it, so this is the shape FanOut exists for: several at a
// time, in the listing's order, and an episode that will not load dropped
// rather than sinking a job of hundreds.
func (a *ABCListen) programme(ctx context.Context, u *url.URL, page *abcListenPage) (*Result, error) {
	episodes := a.episodes(ctx, u, page.props.Collection)
	if len(episodes) == 0 {
		return nil, fmt.Errorf("abclisten: no episodes listed on %s", u.Redacted())
	}

	files := FanOut(ctx, episodes, func(ctx context.Context, ep abcListenItem) ([]File, error) {
		return a.listed(ctx, u, ep)
	})
	if len(files) == 0 {
		return nil, fmt.Errorf("abclisten: none of the %d episodes of %s carry media",
			len(episodes), u.Redacted())
	}

	return &Result{
		Title: util.FirstNonEmpty(page.title, util.NameFromURL(u.Path)),
		Files: files,
	}, nil
}

// listed resolves one entry of a programme listing. The episode goes in a
// folder named after the programme, so unlike a lone episode its filename
// says nothing about which programme it belongs to.
func (a *ABCListen) listed(ctx context.Context, base *url.URL, ep abcListenItem) ([]File, error) {
	link := resolveRef(base, ep.ArticleLink)
	page, err := a.page(ctx, link)
	if err != nil {
		return nil, err
	}
	props := page.props
	if props.restricted() {
		return nil, abcListenAppOnly(link)
	}
	if props.Data == nil {
		return nil, fmt.Errorf("abclisten: %s is not an episode", link)
	}

	doc := props.Data.DocumentProps
	file, ok := a.file(doc)
	if !ok {
		return nil, fmt.Errorf("abclisten: %s carries no audio or video", link)
	}
	file.Name = abcListenName(
		util.FirstNonEmpty(doc.ID, ep.CardID),
		util.FirstNonEmpty(doc.Title, ep.CardTitle)) + file.Name
	return []File{file}, nil
}

// episodes lists a programme's episodes.
//
// The page renders only the first handful and loads the rest from an endpoint
// of its own as the reader pages through; asked directly that endpoint
// answers fifty at a time and reports the true total. Those the page already
// rendered are kept and paging continues after them, which is what makes the
// endpoint additive: if it goes away, a programme resolves to the episodes on
// the page instead of to nothing at all.
func (a *ABCListen) episodes(ctx context.Context, u *url.URL, coll *abcListenCollection) []abcListenItem {
	var out []abcListenItem
	seen := make(map[string]bool)
	add := func(items []abcListenItem) int {
		before := len(out)
		for _, item := range items {
			if item.ArticleLink == "" || seen[item.ArticleLink] {
				continue
			}
			seen[item.ArticleLink] = true
			out = append(out, item)
		}
		return len(out) - before
	}

	add(coll.Items)
	total := coll.Pagination.Total
	endpoint := util.Origin(u) + abcListenEpisodesPath + "/" + strings.Trim(u.EscapedPath(), "/")
	for len(out) < total && len(out) < abcListenMaxEpisodes {
		query := url.Values{
			"collectionId": {coll.ID},
			"offset":       {strconv.Itoa(len(out))},
			"size":         {strconv.Itoa(abcListenPageSize)},
		}
		var body struct {
			Collection abcListenCollection `json:"collection"`
		}
		err := a.client.GetJSON(ctx, endpoint+"?"+query.Encode(),
			httpx.Referer(abcListenRoot+"/"), &body)
		// A listing that stops early, for whatever reason, is still worth
		// having: what is already in hand is the newest episodes, which is
		// the half of a podcast anyone asks for first. A page that adds
		// nothing new ends it too, in case the endpoint ever stops honouring
		// the offset.
		if err != nil || add(body.Collection.Items) == 0 {
			break
		}
		if body.Collection.Pagination.Total > total {
			total = body.Collection.Pagination.Total
		}
	}

	if len(out) > abcListenMaxEpisodes {
		out = out[:abcListenMaxEpisodes]
	}
	return out
}

// page fetches one page and reads its JSON island.
func (a *ABCListen) page(ctx context.Context, link string) (*abcListenPage, error) {
	doc, err := a.client.GetString(ctx, link, httpx.Referer(abcListenRoot+"/"))
	if err != nil {
		return nil, fmt.Errorf("abclisten: fetch %s: %w", link, err)
	}
	page, err := abcListenParse(doc)
	if err != nil {
		return nil, fmt.Errorf("abclisten: %s: %w", link, err)
	}
	return page, nil
}

// abcListenPage is a fetched page: the document it was rendered from, and its
// title, which is the one place a programme's own name is written out in
// words rather than assembled from a heading.
type abcListenPage struct {
	props abcListenPageProps
	title string
}

// abcListenParse reads a fetched page. Kept apart from the fetch so a fixture
// can stand in for the page.
func abcListenParse(doc string) (*abcListenPage, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return nil, err
	}
	island := findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.Script) && attr(n, "id") == abcListenIsland
	})
	if island == nil {
		return nil, fmt.Errorf("no %s island on the page", abcListenIsland)
	}

	// The two wrapper levels the framework writes carry nothing of their own
	// and are unwrapped here rather than being propagated through every use.
	var envelope struct {
		Props struct {
			PageProps abcListenPageProps `json:"pageProps"`
		} `json:"props"`
	}
	if err := json.Unmarshal([]byte(abcListenScriptText(island)), &envelope); err != nil {
		return nil, fmt.Errorf("decode %s: %w", abcListenIsland, err)
	}

	// The site suffix is a fixed string and is trimmed as one: cutting at the
	// first " - " instead, the way most of these hosts are handled, would
	// behead every programme whose own name contains a dash.
	return &abcListenPage{
		props: envelope.Props.PageProps,
		title: strings.TrimSpace(strings.TrimSuffix(
			strings.TrimSpace(firstText(root, atom.Title)), abcListenTitleSuffix)),
	}, nil
}

// abcListenScriptText reads a script element's contents verbatim. textOf
// would do almost as well and is what the other page parsers here use, but it
// collapses runs of whitespace, and this element is JSON: the spacing inside
// a title is data rather than presentation.
func abcListenScriptText(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}

// file turns a document's best rendition into a download. The returned File
// carries only the extension in Name; the caller supplies the stem, which
// differs between a lone episode and one of a programme.
func (a *ABCListen) file(doc abcListenDoc) (File, bool) {
	best, ok := abcListenBest(doc.Renditions)
	if !ok {
		return File{}, false
	}

	// The extension comes from the URL rather than from the MIME type beside
	// it, which is wrong on one entry per AAC episode — see the note on the
	// type.
	headers := httpx.Referer(abcListenRoot + "/")
	file := File{
		Name:    path.Ext(util.NameFromURL(best.URL)),
		URL:     best.URL,
		Size:    -1,
		Headers: headers,
	}
	if best.FileSize > 0 {
		file.Size = best.FileSize
		file.SizeApprox = !best.measured()
	}
	if abcListenGeoBucket(best.URL) {
		file.Resolve = a.geoCheck(best.URL, headers)
	}
	return file, true
}

// abcListenGeoBucket reports a media URL the ABC serves inside Australia
// alone.
//
// Nothing in the document says so. webDistributionRestricted is false on
// these, allowMediaDownload says "all", and the page offers them like
// anything else; the only statement of the restriction anywhere is the first
// path segment of the media URL itself, which reads "audio" for the open
// catalogue and "geo-001" for the rest. That bucket answers a request from
// outside the country with 403 and a web page.
//
// Matched on the prefix rather than on the one bucket seen, so a second
// numbered bucket is recognised rather than silently treated as open.
func abcListenGeoBucket(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	segs := util.PathSegments(u)
	return len(segs) > 0 && strings.HasPrefix(segs[0], abcListenGeoBucketPrefix)
}

// geoCheck asks the CDN whether this caller may have the file, so that a
// refusal arrives as a sentence rather than as a bare 403.
//
// The downloader's own guard for a host's dead end, File.Reject, cannot help
// here: a non-2xx response fails the transfer before Reject is consulted, so
// the only place left to speak is before the transfer starts. That is what
// Resolve is, and asking there costs one single-byte request against a
// download of tens of megabytes — paid only by the handful of episodes that
// sit in a geo bucket at all.
//
// Anything other than an outright refusal hands back the same target and gets
// out of the way, so a probe that fails for its own reasons cannot invent a
// download failure. And the answer is deliberately not remembered: whether
// this caller is in Australia is a fact about the moment rather than about
// the process — a VPN comes and goes — and a cached refusal would go on
// refusing a download that had started working.
func (a *ABCListen) geoCheck(link string, headers httpx.Header) func(context.Context) (*Target, error) {
	return func(ctx context.Context) (*Target, error) {
		target := &Target{URL: link, Headers: headers}
		req, err := a.client.NewRequest(ctx, http.MethodGet, link, nil)
		if err != nil {
			return target, nil
		}
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		req.Header.Set(httpx.HeaderRange, "bytes=0-0")

		resp, err := a.client.DoOnce(req)
		if err != nil {
			return target, nil
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("abclisten: the ABC licenses this recording for "+
				"Australia only, and its content network refuses %s from anywhere else", link)
		}
		return target, nil
	}
}

// abcListenBest picks the rendition to fetch.
//
// Ranked by byte count rather than by bitrate, which is the field that looks
// made for the job: every AAC rendition here reports a bitrate of zero, so
// ranking by that would prefer a forty-megabyte MP3 over the two-hour AAC
// listed beside it. The length is present on nearly everything, orders
// renditions of one episode correctly whatever they are encoded in, and puts
// a video rendition above the audio it shares a page with. Bitrate settles
// the ties it cannot.
func abcListenBest(renditions []abcListenRendition) (abcListenRendition, bool) {
	var best abcListenRendition
	found := false
	for _, r := range renditions {
		// The listing carries a blank entry now and then: every field null
		// and nowhere to fetch it from.
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		if !found || r.FileSize > best.FileSize ||
			(r.FileSize == best.FileSize && r.Bitrate > best.Bitrate) {
			best, found = r, true
		}
	}
	return best, found
}

// abcListenName leads a filename with the document id.
//
// Episode titles are not unique within a programme. A daily bulletin is named
// after its programme every day of the year, and even a four-episode show
// managed two episodes called the same thing. Left to collide in one folder
// the downloader falls back to "(2)", which lands on a different episode the
// next run and defeats the skip that would otherwise leave both alone. The id
// is what the site itself keys an episode on, so a saved file can be traced
// back to the page it came from; it leads rather than trails because ABC's
// ids rise with time, which sorts a programme folder into broadcast order.
func abcListenName(id, title string) string {
	id, title = strings.TrimSpace(id), strings.TrimSpace(title)
	switch {
	case id == "":
		return title
	case title == "":
		return id
	}
	return id + " " + title
}

// abcListenAppOnly is the ABC declining to publish something on the web.
//
// webDistributionRestricted is the field, and a page it is set on carries
// neither media nor an episode list — the audiobooks are the case in point.
// The payload says nothing to a listener about why, so unlike nrk there is no
// sentence to pass through and these words are ours.
func abcListenAppOnly(link string) error {
	return fmt.Errorf("abclisten: the ABC publishes %s through its listen app only, "+
		"so the web page carries nothing to fetch", link)
}

// abcListenPageProps is what a page was rendered from. An episode page fills
// in Data and a programme page fills in Collection; no page fills in both,
// which is what lets one URL shape be told from the other without parsing it.
type abcListenPageProps struct {
	// WebDistributionRestricted marks a programme published through the app
	// alone. It sits here on a programme page and on the document itself on
	// an episode page, and means the same thing in both places.
	WebDistributionRestricted bool `json:"webDistributionRestricted"`
	Data                      *struct {
		DocumentProps abcListenDoc `json:"documentProps"`
	} `json:"data"`
	Collection *abcListenCollection `json:"programCollectionPrepared"`
}

// restricted reports the app-only flag wherever this page happened to carry
// it.
func (p abcListenPageProps) restricted() bool {
	if p.WebDistributionRestricted {
		return true
	}
	return p.Data != nil && p.Data.DocumentProps.WebDistributionRestricted
}

// abcListenDoc is one episode.
type abcListenDoc struct {
	ID                        string               `json:"id"`
	Title                     string               `json:"title"`
	WebDistributionRestricted bool                 `json:"webDistributionRestricted"`
	Renditions                []abcListenRendition `json:"renditions"`
	// Analytics is where the programme is named in words. The document's own
	// programID is a number, and nothing else on an episode page states which
	// programme it belongs to except a navigation link.
	Analytics struct {
		Document struct {
			Program struct {
				Name string `json:"name"`
			} `json:"program"`
		} `json:"document"`
	} `json:"analytics"`
}

// programme names the programme this episode belongs to, or "" when the
// document does not say.
func (d abcListenDoc) programme() string {
	return strings.TrimSpace(d.Analytics.Document.Program.Name)
}

// abcListenRendition is one encoding of an episode.
type abcListenRendition struct {
	URL      string `json:"url"`
	MIMEType string `json:"MIMEType"`
	Bitrate  int    `json:"bitrate"`
	FileSize int64  `json:"fileSize"`
}

// measured reports a published length that is the file's own rather than one
// worked out from the duration.
//
// The current media core states each object's real length: every rendition it
// serves matched Content-Length to the byte, which is what makes setting an
// exact Size here worth doing at all. The older library behind it does not —
// its lengths were short by between one and eighty kilobytes on every episode
// checked — and the only thing separating the two at extraction time is the
// type each writes. The media core writes the registered names listed above;
// the older library writes "audio/mp3", which no registry has ever issued.
//
// So this is a list of what has been checked rather than of what to distrust,
// because the two directions fail differently. A type nobody here has seen
// costs a skip, and a skip costs one request. A length believed and merely
// close would have that skip compare against a number the file can never
// reach, which is the failure the Size contract exists to prevent.
func (r abcListenRendition) measured() bool {
	return abcListenMeasuredTypes[strings.ToLower(strings.TrimSpace(r.MIMEType))]
}

// abcListenMeasuredTypes are the types the current media core labels its
// files with. See abcListenRendition.measured.
var abcListenMeasuredTypes = map[string]bool{
	"audio/mpeg": true,
	"audio/aac":  true,
	"video/mp4":  true,
}

// abcListenCollection is a programme's episode listing, in the shape the page
// and the paging endpoint both use.
type abcListenCollection struct {
	// ID selects the collection, and is the only parameter the paging
	// endpoint actually reads.
	ID    string          `json:"id"`
	Items []abcListenItem `json:"items"`
	// Pagination reports how many episodes there are in total. Its other
	// fields are typed inconsistently between the page and the endpoint — one
	// writes the page size as a number and the other as a string — so nothing
	// here reads them.
	Pagination struct {
		Total int `json:"total"`
	} `json:"pagination"`
}

// abcListenItem is one entry of that listing. It names an episode and links
// to it but carries no media of its own, which is why each one is opened.
type abcListenItem struct {
	ArticleLink string `json:"articleLink"`
	CardTitle   string `json:"cardTitle"`
	CardID      string `json:"cardId"`
}
