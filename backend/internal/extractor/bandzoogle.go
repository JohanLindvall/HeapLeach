package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Bandzoogle installs.
//
// Bandzoogle is website software a musician rents rather than a site anyone
// visits, so every install answers on its owner's own domain and no list of
// hosts could ever name them. That is the same shape Chevereto and Kernel
// Video Sharing have here, and it gets the same two answers: the
// "bandzoogle" family of HEAPLEACH_EXTRA_HOSTS names installs at runtime,
// and a sniff from the direct fallback recognises the software itself.
//
// What makes the sniff cheap is that the pages label their own player. Every
// track is an anchor carrying data-zoogle-track, and beside it the title, the
// artist and the path the audio is served from — so one page fetch yields
// names and links together, and there is nothing to guess at.
//
// The audio link is the site's own /player/<player>/tracks/<track>.mp3, which
// answers with a redirect to storage signed for a couple of hours. That
// signature is minted per request, so the player link is what goes on the
// File: a plain URL that cannot go stale, where storing the signed one would
// leave a queue failing partway down. The same judgement imagepond's /direct
// route gets, for the same reason.
type Bandzoogle struct {
	hostSet
	client *httpx.Client
	name   string
}

// bandzoogleTrackAttr marks a track anchor, and is the software's own doing
// rather than a theme's: it is what the player script binds to.
const bandzoogleTrackAttr = "data-zoogle-track"

// bandzoogleIDAttr identifies the recording, which is what tells one track
// from another when a page lists the same album under two players.
const bandzoogleIDAttr = "data-id"

// bandzoogleDestAttr carries the path the audio is served from.
const bandzoogleDestAttr = "data-dest"

// NewBandzoogleSites builds one extractor per install named at runtime.
//
// There is no built-in list, and that is not an omission: a band's site is on
// the band's domain, so the only installs this can know about are the ones a
// user names. Everything else arrives through the sniff.
func NewBandzoogleSites(cfg *config.Config, client *httpx.Client) []Extractor {
	hosts := util.Dedupe(cfg.ExtraHostsFor(config.FamilyBandzoogle))
	out := make([]Extractor, 0, len(hosts))
	for _, host := range hosts {
		name, _, _ := strings.Cut(host, ".")
		out = append(out, &Bandzoogle{
			hostSet: hostSet{strings.ToLower(host)},
			client:  client,
			name:    name,
		})
	}
	return out
}

func (b *Bandzoogle) Name() string { return b.name }

// Extract reads every track on the page. The host was named outright, so a
// page that cannot be fetched or holds no player is an error rather than
// something to pass over.
func (b *Bandzoogle) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	doc, err := b.client.GetString(ctx, u.String(), httpx.Referer(util.Origin(u)+"/"))
	if err != nil {
		return nil, fmt.Errorf("bandzoogle: fetch %s: %w", u.Redacted(), err)
	}
	return bandzoogleParse(ctx, b.client, doc, u)
}

// bandzoogleMaxPages bounds the walk over a listing's continuations. Far
// above any real album, so it stops a broken page looping rather than
// truncating a long one.
const bandzoogleMaxPages = 20

// bandzoogleParse turns a page that has already been fetched into its tracks,
// following the rest of any listing that says it has more.
//
// A track list shows twenty and says so: data-offset names where it stopped
// and data-load-more whether anything follows. Asking for the same page with
// ?offset=N returns the remainder, itself saying whether more follows again.
// Without this an album of twenty-five arrived as twenty, which is not a
// failure anything reports — the page looks complete and the last five are
// simply absent.
func bandzoogleParse(ctx context.Context, client *httpx.Client, doc string, u *url.URL) (*Result, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("bandzoogle: parse %s: %w", u.Redacted(), err)
	}

	files, artist := bandzoogleTracks(root, u)
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		seen[f.URL] = true
	}

	followed := map[string]bool{"": true}
	pending := bandzoogleMore(root)
	for pages := 0; pages < bandzoogleMaxPages && len(pending) > 0; pages++ {
		offset := pending[0]
		pending = pending[1:]
		if followed[offset] {
			continue
		}
		followed[offset] = true

		next := *u
		q := next.Query()
		q.Set("offset", offset)
		next.RawQuery = q.Encode()

		more, err := client.GetString(ctx, next.String(), httpx.Referer(util.Origin(u)+"/"))
		if err != nil {
			// What is already in hand is worth more than nothing: a listing
			// whose tail will not load is short, not broken.
			break
		}
		moreRoot, err := parseHTML(more)
		if err != nil {
			break
		}
		added, moreArtist := bandzoogleTracks(moreRoot, u)
		artist = util.FirstNonEmpty(artist, moreArtist)
		for _, f := range added {
			if seen[f.URL] {
				continue
			}
			seen[f.URL] = true
			files = append(files, f)
		}
		pending = append(pending, bandzoogleMore(moreRoot)...)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("bandzoogle: no playable tracks on %s — the page "+
			"names a player but nothing it could fetch", u.Redacted())
	}
	title := util.FirstNonEmpty(
		artist,
		trimSiteSuffix(firstText(root, atom.Title)),
		u.Hostname(),
	)
	return &Result{Title: title, Files: files}, nil
}

// bandzoogleMore reports the offsets of listings that say they have more to
// give. A list that has shown everything says so, which is what ends the walk.
func bandzoogleMore(root *html.Node) []string {
	var out []string
	for _, list := range findAll(root, func(n *html.Node) bool {
		return isElem(n, atom.Ol) && hasClass(n, "track-list")
	}) {
		if !strings.EqualFold(attr(list, "data-load-more"), "true") {
			continue
		}
		offset := strings.TrimSpace(attr(list, "data-offset"))
		if _, err := strconv.Atoi(offset); err != nil {
			continue
		}
		out = append(out, offset)
	}
	return util.Dedupe(out)
}

// bandzoogleTracks reads the track anchors, and reports the artist they name.
//
// The anchor is the whole record: the software prints the title, the artist
// and the audio path onto the element its player binds to, so nothing has to
// be recovered from the surrounding markup — which is a theme's business and
// varies from one band's site to the next.
func bandzoogleTracks(root *html.Node, base *url.URL) (files []File, artist string) {
	seen := make(map[string]bool)
	for _, a := range findAll(root, isBandzoogleTrack) {
		link := resolveRef(base, attr(a, bandzoogleDestAttr))
		if link == "" {
			continue
		}
		// Keyed on the track's own id, not on the link. A page carries a
		// player per album and the same recording appears under more than
		// one of them, which makes the *paths* differ — /player/A/tracks/9
		// and /player/B/tracks/9 — while the recording is the one thing they
		// have in common. Keyed on the link, one album listed twice queued
		// every track twice: 44 entries for 24 songs, each repeat fetched
		// only to be dropped by the already-on-disk check.
		key := strings.TrimSpace(attr(a, bandzoogleIDAttr))
		if key == "" {
			key = link
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		if artist == "" {
			artist = strings.TrimSpace(attr(a, "data-artist"))
		}
		// The path ends in .mp3, so a title needs the extension put back;
		// a track the page did not name falls back to that path's own.
		name := strings.TrimSpace(attr(a, "data-title"))
		if name != "" {
			name = bandzoogleNumber(a) + name + bandzoogleExt(link)
		}
		files = append(files, File{
			Name: util.FirstNonEmpty(name, util.NameFromURL(link)),
			// Foldered by album, because the numbering restarts with each
			// one. A page carrying two albums puts two track 1s in the same
			// directory otherwise, where they sort next to each other and
			// the numbers say nothing about either.
			Dir:     bandzoogleAlbum(a),
			URL:     link,
			Size:    -1,
			Headers: httpx.Referer(util.Origin(base) + "/"),
		})
	}
	return files, artist
}

// bandzoogleAlbum names the album a track sits in, or "" when the page does
// not group it under one.
//
// The name is a heading, and headings are a theme's business rather than the
// software's, so this asks for the nearest one that shares an ancestor with
// the track — and stops the moment that ancestor holds more than one player.
// Each album has a player of its own, so two of them means the walk has
// climbed into whatever section encloses several albums, whose heading names
// none of them. Without that guard a track would be filed under "Discography".
func bandzoogleAlbum(a *html.Node) string {
	for n, depth := a.Parent, 0; n != nil && depth < 12; n, depth = n.Parent, depth+1 {
		if bandzooglePlayers(n) > 1 {
			return ""
		}
		heading := findFirst(n, func(c *html.Node) bool {
			switch {
			case isElem(c, atom.H1), isElem(c, atom.H2), isElem(c, atom.H3),
				isElem(c, atom.H4), isElem(c, atom.H5), isElem(c, atom.H6):
				return strings.TrimSpace(textOf(c)) != ""
			}
			return false
		})
		if heading != nil {
			return bandzoogleAlbumName(textOf(heading))
		}
	}
	return ""
}

// bandzoogleAlbumName tidies a heading into something worth being a folder.
//
// These headings wrap the release in typographic quotes — EP „Name“ — which
// say nothing a directory needs and read as noise in a file browser. Only the
// quoting characters go: apostrophes stay, because those sit inside words
// rather than around them, and a title that owns one would be left misspelt.
func bandzoogleAlbumName(heading string) string {
	return collapseSpace(bandzoogleDecoration.Replace(heading))
}

// bandzoogleDecoration drops the marks used to quote a title, in the several
// spellings the web writes them.
var bandzoogleDecoration = strings.NewReplacer(
	`„`, "", `“`, "", `”`, "", `"`, "",
	`«`, "", `»`, "", `‹`, "", `›`, "",
	`‟`, "", `〝`, "", `〞`, "",
)

// bandzooglePlayers counts the distinct players whose tracks sit under n,
// which is how the walk above knows it has left one album's markup.
func bandzooglePlayers(n *html.Node) int {
	seen := make(map[string]bool)
	for _, a := range findAll(n, isBandzoogleTrack) {
		u, err := url.Parse(attr(a, bandzoogleDestAttr))
		if err != nil {
			continue
		}
		segs := util.PathSegments(u)
		if len(segs) >= 2 && segs[0] == "player" {
			seen[segs[1]] = true
		}
	}
	return len(seen)
}

// bandzoogleNumber is the track's position on the page, as a filename
// prefix, or "" when the page does not print one.
//
// The number is not on the anchor — the software puts it in a sibling span —
// so the walk goes up from the anchor looking for one. It stops the moment it
// reaches an element holding more than a single track, because the list that
// encloses them all contains every number and would hand back a neighbour's.
//
// Padded to two digits so a directory listing sorts the way the album plays,
// which a bare "2" beside a "10" does not. Numbering restarts per album and
// several albums share a page, but a title never appears under two different
// numbers here, so this neither creates collisions nor breaks the skip that
// already folds the repeats together.
func bandzoogleNumber(a *html.Node) string {
	for n, depth := a.Parent, 0; n != nil && depth < 3; n, depth = n.Parent, depth+1 {
		if len(findAll(n, isBandzoogleTrack)) > 1 {
			return ""
		}
		span := findFirst(n, func(c *html.Node) bool { return hasClass(c, "track-number") })
		if span == nil {
			continue
		}
		digits := strings.TrimSpace(textOf(span))
		position, err := strconv.Atoi(digits)
		if err != nil || position <= 0 {
			return ""
		}
		return fmt.Sprintf("%02d ", position)
	}
	return ""
}

// isBandzoogleTrack reports whether a node is one of the player's track
// anchors, which is both how they are found and how the number walk above
// knows it has climbed past the track it started from.
func isBandzoogleTrack(n *html.Node) bool {
	return isElem(n, atom.A) && attr(n, bandzoogleTrackAttr) != ""
}

// bandzoogleExt is the audio extension the link carries, defaulting to .mp3
// — which is what these serve, and better than a file with no extension at
// all if the path ever stops saying so.
func bandzoogleExt(link string) string {
	if name := util.NameFromURL(link); name != "" {
		if i := strings.LastIndexByte(name, '.'); i > 0 {
			return name[i:]
		}
	}
	return ".mp3"
}

// bandzoogleSniff recognises the software on a host nobody listed.
//
// Two things it must not do, both of which it did at first and both of which
// broke downloads that have nothing to do with this platform. It must not
// fetch a URL that names a file: that URL *is* a file, and reading it as a
// page spends a request on every direct link anyone pastes. And a fetch that
// fails is not a claim — it is not having managed to look — so it falls
// through to whatever would have happened anyway, rather than reporting this
// software's name over somebody else's 404.
//
// Once the marker is seen it commits, because falling through then would
// hand the page to the direct fallback, which would write the HTML to disk
// and call it a finished download. See directSniff.
func bandzoogleSniff(ctx context.Context, client *httpx.Client, u *url.URL) (*Result, error) {
	if path.Ext(u.Path) != "" {
		return nil, nil
	}
	doc, err := client.GetString(ctx, u.String(), httpx.Referer(util.Origin(u)+"/"))
	if err != nil {
		return nil, nil
	}
	if !strings.Contains(doc, bandzoogleTrackAttr) {
		return nil, nil
	}
	return bandzoogleParse(ctx, client, doc, u)
}
