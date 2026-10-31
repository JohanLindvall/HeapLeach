// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

// Niconico reads niconico's own player API.
//
// A watch page describes its video in the JSON of a <meta
// name="server-response">: the qualities it is offered in (media.domand,
// video and audio apart, each marked available or not to a caller without
// an account), a key to ask for them with (accessRightKey), and the id the
// player reports itself by (client.watchTrackId). Asked for one video and
// one audio quality, the access-rights API mints an HLS master for the pair
// and sets the domand_bid cookie the key server wants: the segments are on
// signed links anyone can fetch, but the AES-128 key is refused without it.
// The audio is a rendition of its own, and the segments are fragmented MP4
// under AES-128 with the initialisation segment in the clear, which the
// downloader joins and decrypts (download/mux.go, hlscrypt.go). It used to
// be handed to yt-dlp for the muxing.
//
// The grant and the cookie last a day, and a queue can wait longer, so both
// are minted when the transfer starts (Resolve), from a fresh reading of the
// watch page. Reading it when the job is added as well is what tells a
// removed or private video apart before then.
//
// Mylists, series and a user's uploads are read through the same API, which
// names each video and its title, so a listing costs one request a page
// rather than one a video. Anything else on the site is still handed to
// yt-dlp, as all of it was.
type Niconico struct {
	about
	client *httpx.Client
	// watch and api are where the watch pages and the API are, fields so
	// that tests can stand in for them.
	watch, api string
	handoff    *Handoff
}

const (
	niconicoWatchRoot = "https://www.nicovideo.jp/watch/"
	niconicoAPIRoot   = "https://nvapi.nicovideo.jp"
	niconicoOrigin    = "https://www.nicovideo.jp"
	// niconicoPageSize is the most a listing hands out a page.
	niconicoPageSize = 100
)

// niconicoRest is the part of the site still handed to yt-dlp.
var niconicoRest = handoffSite{
	name: "niconico",
	why:  "only its videos, mylists, series and uploads are read natively, not a page like this one",
}

// NewNiconico builds the niconico extractor.
func NewNiconico(client *httpx.Client) *Niconico {
	return &Niconico{
		about: about{name: "niconico", shape: eitherShape(niconicoWatchShape, niconicoListShape),
			recognises: "watch pages (`/watch/<id>`) describing their video in a `server-response` meta element, " +
				"and mylists, series and users' uploads rendered by the niconico web client's bundle; " +
				"live watch pages describing their program in the live client's embedded data are handed to yt-dlp"},
		client: client, watch: niconicoWatchRoot, api: niconicoAPIRoot,
		handoff: newHandoff(client, niconicoRest),
	}
}

var (
	// niconicoWatchShape is a watch page, and the short link to one.
	niconicoWatchShape = pathShape(`^/watch/(?:[a-z]{2})?[0-9]+/?$`, `^/(?:sm|nm|so)[0-9]+/?$`)
	// niconicoListShape is a mylist, a series or a user's uploads.
	niconicoListShape = pathShape(
		`^/(?:mylist|series)/[0-9]+/?$`,
		`^/user/[0-9]+(?:/video|/(?:mylist|series)/[0-9]+)?/?$`,
	)
)

// niconicoNames are what the site calls itself: the video service, and the
// portal its user pages belong to.
var niconicoNames = []string{"ニコニコ動画", "ニコニコ", "niconico"}

// niconicoResponseMeta is the meta element a watch page describes its
// video in, which is what this extractor reads.
const niconicoResponseMeta = `name="server-response"`

// niconicoBundle is the web client's script bundle, which renders the
// pages that carry no description of their own: a user's, a mylist.
const niconicoBundle = "/web/scripts/bundle/"

// Recognise takes a page of the site by what this extractor reads on it: a
// watch page by its server-response description, which a short link
// redirects to, and a listing by the web client that renders it. Any other
// page of the site that carries either is the handoff's, as all of it was
// before videos and listings were read natively.
func (n *Niconico) Recognise(_ context.Context, p *Page) bool {
	if niconicoLivePage(p) {
		return true
	}
	if !p.Named(niconicoNames...) {
		return false
	}
	return p.Has(niconicoResponseMeta) || p.Script(niconicoBundle)
}

// Live programs use another player and API. Their state identifies the
// requested program; neither branding nor a watch-shaped path alone does.
func niconicoLivePage(p *Page) bool {
	segs := p.Segments()
	if len(segs) != 2 || segs[0] != "watch" || !strings.HasPrefix(segs[1], "lv") || !allDigits(strings.TrimPrefix(segs[1], "lv")) ||
		!p.Script("/relive/program-watch/scripts/") {
		return false
	}
	return p.hasElement(func(node *html.Node) bool {
		if node.Type != html.ElementNode || node.Data != "script" || attr(node, "id") != "embedded-data" {
			return false
		}
		var state struct {
			Program struct {
				ID string `json:"nicoliveProgramId"`
			} `json:"program"`
		}
		return json.Unmarshal([]byte(attr(node, "data-props")), &state) == nil && state.Program.ID == segs[1]
	})
}

// The kinds of page read natively.
const (
	niconicoVideo = iota + 1
	niconicoMylist
	niconicoSeries
	niconicoUploads
)

// niconicoVideoID is a video's id: digits, after a two-letter prefix where
// the id has one.
var niconicoVideoID = regexp.MustCompile(`^(?:[a-z]{2})?[0-9]+$`)

// niconicoLink tells what a link names: a video, a mylist, a series, a
// user's uploads, or none of those (0).
func niconicoLink(u *url.URL) (kind int, id string) {
	segs := util.PathSegments(u)
	if len(segs) > 0 && strings.HasPrefix(segs[len(segs)-1], "lv") &&
		(len(segs) == 1 || len(segs) == 2 && segs[0] == "watch") {
		return 0, ""
	}
	if util.HostMatches(u.Host, "nico.ms") {
		if len(segs) == 1 && niconicoVideoID.MatchString(segs[0]) {
			return niconicoVideo, segs[0]
		}
		return 0, ""
	}
	if len(segs) >= 2 && segs[0] == "user" && allDigits(segs[1]) {
		switch {
		case len(segs) == 2, len(segs) == 3 && segs[2] == "video":
			return niconicoUploads, segs[1]
		case len(segs) == 4 && segs[2] == "mylist":
			segs = segs[2:]
		case len(segs) == 4 && segs[2] == "series":
			segs = segs[2:]
		}
	}
	if len(segs) == 2 {
		switch {
		case segs[0] == "watch" && niconicoVideoID.MatchString(segs[1]):
			return niconicoVideo, segs[1]
		case segs[0] == "mylist" && allDigits(segs[1]):
			return niconicoMylist, segs[1]
		case segs[0] == "series" && allDigits(segs[1]):
			return niconicoSeries, segs[1]
		}
	}
	return 0, ""
}

// Extract reads a video, or a listing of them, and hands anything else on
// the site to yt-dlp.
func (n *Niconico) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	kind, id := niconicoLink(u)
	switch kind {
	case niconicoVideo:
		watch, err := n.watchPage(ctx, id)
		if err != nil {
			return nil, err
		}
		return &Result{Title: watch.title, Files: []File{n.file(id, watch.title)}}, nil
	case niconicoMylist:
		return n.list(ctx, opts, "mylist "+id, n.mylistPage(id))
	case niconicoSeries:
		return n.list(ctx, opts, "series "+id, n.seriesPage(id))
	case niconicoUploads:
		return n.list(ctx, opts, "user "+id, n.uploadsPage(id))
	}
	return n.handoff.Extract(ctx, u, opts)
}

// file is a video's file, its stream minted when the transfer starts.
func (n *Niconico) file(id, title string) File {
	return File{
		Name:    title + ".mp4",
		Size:    -1,
		Headers: httpx.SiteOrigin(niconicoOrigin),
		Resolve: func(ctx context.Context) (*Target, error) { return n.stream(ctx, id) },
	}
}

// niconicoStream is one quality a video is offered in.
type niconicoStream struct {
	ID          string `json:"id"`
	IsAvailable bool   `json:"isAvailable"`
	BitRate     int    `json:"bitRate"`
	Height      int    `json:"height"`
}

// niconicoServerResponse is the part of a watch page's description read
// here.
type niconicoServerResponse struct {
	Meta struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	} `json:"meta"`
	Data struct {
		Response struct {
			ErrorCode  string `json:"errorCode"`
			ReasonCode string `json:"reasonCode"`
			Video      struct {
				Title string `json:"title"`
			} `json:"video"`
			Media struct {
				Domand *struct {
					Videos         []niconicoStream `json:"videos"`
					Audios         []niconicoStream `json:"audios"`
					AccessRightKey string           `json:"accessRightKey"`
				} `json:"domand"`
			} `json:"media"`
			Client struct {
				WatchTrackID string `json:"watchTrackId"`
			} `json:"client"`
		} `json:"response"`
	} `json:"data"`
}

// niconicoWatch is one reading of a watch page.
type niconicoWatch struct {
	title, video, audio, key, track string
}

// errNiconicoNoStream is a video offered to nobody without an account.
var errNiconicoNoStream = errors.New("offers no quality to a viewer without a niconico account " +
	"(it may need one, or a payment)")

// watchPage reads a video's watch page: its title, the best quality of each
// kind it offers, and what to ask for them with. A page that is an error
// says so in the same description, behind a 4xx, and that is read instead.
func (n *Niconico) watchPage(ctx context.Context, id string) (*niconicoWatch, error) {
	watchURL, err := url.Parse(n.watch + url.PathEscape(id))
	if err != nil {
		return nil, fmt.Errorf("niconico: %s: %w", id, err)
	}
	page, _, err := fetchDocument(ctx, n.client, watchURL, httpx.Referer(niconicoOrigin+"/"))
	if err != nil {
		status, ok := errors.AsType[*httpx.StatusError](err)
		if !ok || !strings.Contains(status.Body, "server-response") {
			return nil, fmt.Errorf("niconico: %s: %w", id, err)
		}
		page = status.Body
	}
	root, err := parseHTML(page)
	if err != nil {
		return nil, fmt.Errorf("niconico: %s: %w", id, err)
	}
	var doc niconicoServerResponse
	if raw := metaContent(root, "server-response"); raw == "" {
		return nil, fmt.Errorf("niconico: %s: the watch page carries no description of the video", id)
	} else if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("niconico: %s: read the watch page's description: %w", id, err)
	}
	resp := doc.Data.Response
	switch code := cmp.Or(resp.ErrorCode, doc.Meta.Code); {
	case code == "NOT_FOUND":
		return nil, fmt.Errorf("niconico: %s does not exist", id)
	case doc.Meta.Status >= 400 || resp.ErrorCode != "":
		return nil, fmt.Errorf("niconico: %s cannot be watched: %s", id, cmp.Or(resp.ReasonCode, code, strconv.Itoa(doc.Meta.Status)))
	}
	domand := resp.Media.Domand
	if domand == nil || domand.AccessRightKey == "" {
		return nil, fmt.Errorf("niconico: %s %w", id, errNiconicoNoStream)
	}
	video, okVideo := niconicoBest(domand.Videos)
	audio, okAudio := niconicoBest(domand.Audios)
	if !okVideo || !okAudio {
		return nil, fmt.Errorf("niconico: %s %w", id, errNiconicoNoStream)
	}
	return &niconicoWatch{
		title: cmp.Or(strings.TrimSpace(resp.Video.Title), id),
		video: video, audio: audio, key: domand.AccessRightKey, track: resp.Client.WatchTrackID,
	}, nil
}

// niconicoBest is the best quality of a kind offered without an account:
// the tallest, then the highest bitrate.
func niconicoBest(streams []niconicoStream) (string, bool) {
	best := -1
	for i, s := range streams {
		if !s.IsAvailable || s.ID == "" {
			continue
		}
		if best < 0 || cmp.Or(cmp.Compare(s.Height, streams[best].Height), cmp.Compare(s.BitRate, streams[best].BitRate)) > 0 {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	return streams[best].ID, true
}

// stream mints a video's stream: a fresh reading of its watch page, a grant
// for its best qualities, and the HLS master the grant names.
func (n *Niconico) stream(ctx context.Context, id string) (*Target, error) {
	watch, err := n.watchPage(ctx, id)
	if err != nil {
		return nil, err
	}
	master, cookie, err := n.grant(ctx, id, watch)
	if err != nil {
		return nil, err
	}
	headers := httpx.SiteOrigin(niconicoOrigin)
	if cookie != "" {
		headers[httpx.HeaderCookie] = "domand_bid=" + cookie
	}
	media, err := resolveMediaPlaylist(ctx, n.client, master, headers)
	if err != nil {
		return nil, fmt.Errorf("niconico: %s: %w", id, err)
	}
	return media.target(master, watch.title+media.extension(), headers), nil
}

// grant asks the access-rights API for an HLS master of the chosen pair of
// qualities, returning it and the domand_bid cookie the answer sets.
func (n *Niconico) grant(ctx context.Context, id string, watch *niconicoWatch) (master, cookie string, err error) {
	body, err := json.Marshal(map[string][][]string{"outputs": {{watch.video, watch.audio}}})
	if err != nil {
		return "", "", err
	}
	endpoint := n.api + "/v1/watch/" + url.PathEscape(id) + "/access-rights/hls?" +
		url.Values{"actionTrackId": {watch.track}}.Encode()
	req, err := n.client.NewRequest(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return "", "", err
	}
	req.Header.Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
	req.Header.Set(httpx.HeaderAccept, httpx.AcceptJSON)
	req.Header.Set("X-Access-Right-Key", watch.key)
	niconicoAPIHeaders().Apply(req)
	resp, err := n.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("niconico: %s: ask for the stream: %w", id, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, config.MaxResponseBytes))
	if err != nil {
		return "", "", fmt.Errorf("niconico: %s: ask for the stream: %w", id, err)
	}
	var answer struct {
		Data struct {
			ContentURL string `json:"contentUrl"`
		} `json:"data"`
	}
	if resp.StatusCode/100 != 2 || json.Unmarshal(raw, &answer) != nil || answer.Data.ContentURL == "" {
		return "", "", fmt.Errorf("niconico: %s: the stream was not granted (%s: %s)", id, resp.Status,
			util.Truncate(string(raw), 200))
	}
	for _, c := range resp.Cookies() {
		if c.Name == "domand_bid" {
			cookie = c.Value
		}
	}
	return answer.Data.ContentURL, cookie, nil
}

// niconicoAPIHeaders are what every API request carries: the API answers
// only callers naming the site's own front end.
func niconicoAPIHeaders() httpx.Header {
	return httpx.Header{
		"X-Frontend-Id":      "6",
		"X-Frontend-Version": "0",
		"X-Request-With":     niconicoOrigin,
		httpx.HeaderOrigin:   niconicoOrigin,
		httpx.HeaderReferer:  niconicoOrigin + "/",
	}
}

// niconicoEntry is one video of a listing, as the API names it.
type niconicoEntry struct {
	ID                string `json:"id"`
	Title             string `json:"title"`
	IsPaymentRequired bool   `json:"isPaymentRequired"`
	Owner             struct {
		Name string `json:"name"`
	} `json:"owner"`
	// gone marks a video a listing still names after it has gone from the
	// site, or been made private.
	gone bool
}

// niconicoPage is one page of a listing: its name, the videos on it, and
// how many it holds in all.
type niconicoPage struct {
	title   string
	entries []niconicoEntry
	total   int
	more    bool
}

// list walks a listing a page at a time into one file per video, stopping
// at the file cap. A video gone from the site, and one only an account or a
// payment could fetch, are left out, and the note counts them apart from
// what the cap left.
func (n *Niconico) list(ctx context.Context, opts Options, label string,
	page func(context.Context, int) (*niconicoPage, error)) (*Result, error) {
	var (
		title         string
		total         int
		files         []File
		skipped, gone int
	)
	limit := opts.maxFiles()
	// The walk stops once the pages read hold as many playable videos as
	// the file cap: what is left out does not count towards it. The note
	// for the cap is made below, from the total.
	entries, walkNote, err := walkPages(ctx, pageWalk[niconicoEntry]{
		fetch: func(ctx context.Context, number int) (pageOf[niconicoEntry], error) {
			p, err := page(ctx, number+1)
			if err != nil {
				return pageOf[niconicoEntry]{}, err
			}
			title, total = cmp.Or(title, p.title), max(total, p.total)
			var entries []niconicoEntry
			for _, e := range p.entries {
				if e.ID != "" {
					entries = append(entries, e)
				}
			}
			return pageOf[niconicoEntry]{items: entries, next: p.more, end: len(entries) == 0}, nil
		},
		key:   func(e niconicoEntry) string { return e.ID },
		limit: limit,
		weigh: func(entries []niconicoEntry) int {
			playable := 0
			for _, e := range entries {
				if !e.gone && !e.IsPaymentRequired {
					playable++
				}
			}
			return playable
		},
	})
	if err != nil {
		return nil, fmt.Errorf("niconico: %s: %w", label, err)
	}
	for _, e := range entries {
		switch {
		case e.gone:
			gone++
		case e.IsPaymentRequired:
			skipped++
		case len(files) < limit:
			files = append(files, n.file(e.ID, cmp.Or(strings.TrimSpace(e.Title), e.ID)))
		}
	}
	if len(files) == 0 {
		if skipped > 0 {
			return nil, fmt.Errorf("niconico: %s: every video in it needs an account or a payment", label)
		}
		return nil, fmt.Errorf("niconico: %s lists no videos it still has", label)
	}
	// The total counts what was left out too, so only what is left over
	// after those is what the cap cut.
	capped := ""
	if total > len(files)+skipped+gone {
		capped = cappedNote(len(files), total, "videos")
	}
	note := joinNotes(capped, leftOutNote(skipped, " that need an account or a payment"),
		countNote(gone, "no longer on the site"), walkNote)
	return &Result{Title: cmp.Or(title, label), Files: files, Note: note}, nil
}

// apiGet fetches one API document, saying what a 403 or 404 there means.
func (n *Niconico) apiGet(ctx context.Context, endpoint string, out any) error {
	if err := n.client.GetJSON(ctx, endpoint, niconicoAPIHeaders(), out); err != nil {
		if httpx.HasStatus(err, http.StatusForbidden, http.StatusNotFound) {
			return errors.New("it is private, or does not exist")
		}
		return err
	}
	return nil
}

// mylistPage reads one page of a mylist.
func (n *Niconico) mylistPage(id string) func(context.Context, int) (*niconicoPage, error) {
	return func(ctx context.Context, number int) (*niconicoPage, error) {
		var doc struct {
			Data struct {
				Mylist struct {
					Name           string `json:"name"`
					TotalItemCount int    `json:"totalItemCount"`
					HasNext        bool   `json:"hasNext"`
					Items          []struct {
						Status string        `json:"status"`
						Video  niconicoEntry `json:"video"`
					} `json:"items"`
				} `json:"mylist"`
			} `json:"data"`
		}
		endpoint := fmt.Sprintf("%s/v2/mylists/%s?pageSize=%d&page=%d", n.api, url.PathEscape(id), niconicoPageSize, number)
		if err := n.apiGet(ctx, endpoint, &doc); err != nil {
			return nil, err
		}
		mylist := doc.Data.Mylist
		p := &niconicoPage{title: mylist.Name, total: mylist.TotalItemCount, more: mylist.HasNext}
		for _, item := range mylist.Items {
			// A video since removed or made private stays listed, marked so.
			entry := item.Video
			entry.gone = item.Status != "" && item.Status != "public"
			p.entries = append(p.entries, entry)
		}
		return p, nil
	}
}

// seriesPage reads one page of a series.
func (n *Niconico) seriesPage(id string) func(context.Context, int) (*niconicoPage, error) {
	return func(ctx context.Context, number int) (*niconicoPage, error) {
		var doc struct {
			Data struct {
				Detail struct {
					Title string `json:"title"`
				} `json:"detail"`
				TotalCount int `json:"totalCount"`
				Items      []struct {
					Video niconicoEntry `json:"video"`
				} `json:"items"`
			} `json:"data"`
		}
		endpoint := fmt.Sprintf("%s/v2/series/%s?pageSize=%d&page=%d", n.api, url.PathEscape(id), niconicoPageSize, number)
		if err := n.apiGet(ctx, endpoint, &doc); err != nil {
			return nil, err
		}
		p := &niconicoPage{title: doc.Data.Detail.Title, total: doc.Data.TotalCount}
		for _, item := range doc.Data.Items {
			p.entries = append(p.entries, item.Video)
		}
		p.more = number*niconicoPageSize < p.total
		return p, nil
	}
}

// uploadsPage reads one page of a user's uploads, newest first, named for
// the user.
func (n *Niconico) uploadsPage(id string) func(context.Context, int) (*niconicoPage, error) {
	return func(ctx context.Context, number int) (*niconicoPage, error) {
		var doc struct {
			Data struct {
				TotalCount int `json:"totalCount"`
				Items      []struct {
					Essential niconicoEntry `json:"essential"`
				} `json:"items"`
			} `json:"data"`
		}
		endpoint := fmt.Sprintf("%s/v3/users/%s/videos?sortKey=registeredAt&sortOrder=desc&pageSize=%d&page=%d",
			n.api, url.PathEscape(id), niconicoPageSize, number)
		if err := n.apiGet(ctx, endpoint, &doc); err != nil {
			return nil, err
		}
		p := &niconicoPage{total: doc.Data.TotalCount}
		for _, item := range doc.Data.Items {
			p.entries = append(p.entries, item.Essential)
			p.title = cmp.Or(p.title, strings.TrimSpace(item.Essential.Owner.Name))
		}
		p.more = number*niconicoPageSize < p.total
		return p, nil
	}
}
