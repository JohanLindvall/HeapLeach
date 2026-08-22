package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Imgur resolves posts and albums, and the account, tag and mirrored-subreddit
// listings that name them.
//
// None of it goes through the documented API, and none of it could: every
// api.imgur.com/post/v1 endpoint answers an unregistered caller with 401 —
// with no Authorization header, and with an unregistered client id alike — and
// that includes the album and media reads a downloader would want. What
// replaces them is better anyway. A post page assigns the API's own response
// to a global before rendering it, so one ordinary page fetch, with no key and
// no session, yields the same object the album endpoint would have returned:
// the media list and each file's exact byte length.
//
// /gallery/<id> is the route that always carries it, and that is why it is the
// route used whatever the user pasted: /a/<id> answers the site's not-found
// shell for a single-media gallery post, and the bare page /<id> answers it
// for an album. Each works for some posts; /gallery/<id> works for all of
// them, and rebuilding the URL from the id costs nothing.
//
// Listings are the site's older JSON, which is still served to anyone. Its
// rows fall into two kinds, and the distinction is worth keeping: a row for an
// album names a post and nothing else, so its page has to be read; a row from
// a mirrored subreddit is itself the media, extension and exact length
// included, and needs no request at all. Reading the first kind is a request
// per post, so they are read several at a time — a creator with a few hundred
// posts would otherwise spend minutes resolving before a byte moved — and
// bounded by config.MaxListingPosts, because the site does rate-limit a client
// that asks for thousands of pages in a row and a tag feed will happily supply
// them.
type Imgur struct {
	client *httpx.Client
}

const (
	imgurRoot = "https://imgur.com"
	// imgurMediaHost serves the files themselves.
	imgurMediaHost = "i.imgur.com"

	// imgurPostData is the global a post page assigns its own API response
	// to, just before the markup that renders it.
	imgurPostData = "window.postDataJSON"

	// imgurRemovedPath is what the media host substitutes for a file that is
	// gone. See imgurUsableLink for why it has to be refused here.
	imgurRemovedPath = "/removed.png"

	// imgurGIFExt and imgurMP4Ext are the two renditions the site keeps of
	// every animated upload.
	imgurGIFExt = ".gif"
	imgurMP4Ext = ".mp4"
)

// The sections of an account that are listed separately.
const (
	imgurSubmitted = "submitted"
	imgurFavorites = "favorites"
)

// errNoImgurPost marks a page that carries no post data. That is how the site
// answers a bare media id — an image inside an album, or one mirrored from a
// subreddit — as well as an id that never existed, so it is a signal to look
// elsewhere rather than a failure.
var errNoImgurPost = errors.New("the page carries no post data")

// NewImgur builds the imgur extractor.
func NewImgur(client *httpx.Client) *Imgur { return &Imgur{client: client} }

func (i *Imgur) Name() string { return "imgur" }

func (i *Imgur) Match(u *url.URL) bool {
	// i.imgur.com serves the bytes, so a link there is already direct and
	// belongs to the fallback; nothing here would improve on fetching it.
	return util.HostMatches(u.Host, "imgur.com") && !util.HostMatches(u.Host, imgurMediaHost)
}

// Extract resolves a post, an album, or a listing of them.
func (i *Imgur) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	target := imgurTargetOf(util.PathSegments(u))
	switch {
	case target.Listing != "":
		return i.listing(ctx, target)
	case target.ID != "":
		return i.post(ctx, target.ID)
	}
	return nil, fmt.Errorf("imgur: %s names no post (/a/<id>, /gallery/<slug>-<id>), "+
		"account (/user/<name>) or tag (/t/<tag>)", u.Redacted())
}

// ------------------------------------------------------------------ routing

// imgurTarget is what an input URL names.
type imgurTarget struct {
	// ID is a post or media id.
	ID string
	// Listing is the site path whose JSON lists posts, for an account, a tag
	// or a mirrored subreddit.
	Listing string
	// Title names the job a listing produces.
	Title string
}

// imgurTargetOf reads what a path names.
//
// Every shape reduces to an id or a listing path rather than to a URL to
// fetch, because the page that reliably carries a post's data is /gallery/<id>
// however the reader arrived at it.
func imgurTargetOf(segs []string) imgurTarget {
	switch {
	case len(segs) == 0:
		return imgurTarget{}

	case segs[0] == "user" && len(segs) >= 2:
		// /user/<name>, /user/<name>/submitted, /user/<name>/favorites
		section, title := imgurSubmitted, segs[1]
		if len(segs) >= 3 && segs[2] == imgurFavorites {
			section, title = imgurFavorites, segs[1]+" "+imgurFavorites
		}
		return imgurTarget{
			Listing: "/user/" + url.PathEscape(segs[1]) + "/" + section,
			Title:   title,
		}

	case segs[0] == "t" && len(segs) == 2:
		return imgurTarget{Listing: "/t/" + url.PathEscape(segs[1]), Title: "tag " + segs[1]}
	case segs[0] == "r" && len(segs) == 2:
		return imgurTarget{Listing: "/r/" + url.PathEscape(segs[1]), Title: "subreddit " + segs[1]}

	// A tag or subreddit with a post after it names that post, not the list.
	case (segs[0] == "t" || segs[0] == "r") && len(segs) >= 3:
		return imgurTarget{ID: imgurID(segs[2])}

	case (segs[0] == "a" || segs[0] == "gallery") && len(segs) >= 2:
		return imgurTarget{ID: imgurID(segs[1])}

	case len(segs) == 1:
		return imgurTarget{ID: imgurID(segs[0])}
	}
	return imgurTarget{}
}

// imgurID reads the id out of a path segment. The site writes a post's link as
// <slug>-<id> with the slug taken from the title, so the id is the last
// hyphen-separated token; ids themselves are alphanumeric and never carry one.
func imgurID(seg string) string {
	if n := strings.LastIndexByte(seg, '-'); n >= 0 {
		seg = seg[n+1:]
	}
	return seg
}

// --------------------------------------------------------------------- post

// post resolves one post, album or bare media id.
func (i *Imgur) post(ctx context.Context, id string) (*Result, error) {
	files, title, err := i.postFiles(ctx, id)
	if err != nil {
		return nil, err
	}
	return &Result{Title: title, Files: files}, nil
}

// postFiles reads one post's media, falling back to the single-media page for
// an id that is not a post.
func (i *Imgur) postFiles(ctx context.Context, id string) ([]File, string, error) {
	page := imgurRoot + "/gallery/" + url.PathEscape(id)
	doc, err := i.client.GetString(ctx, page, httpx.Referer(imgurRoot+"/"))
	if err != nil {
		return nil, "", fmt.Errorf("imgur: fetch %s: %w", page, err)
	}

	post, err := imgurPostFrom(doc)
	switch {
	case errors.Is(err, errNoImgurPost):
		return i.singleMedia(ctx, id)
	case err != nil:
		return nil, "", fmt.Errorf("imgur: %s: %w", page, err)
	}

	files := imgurFilesOf(post.Media)
	if len(files) == 0 {
		return nil, "", fmt.Errorf("imgur: %s lists no media (every file in it may have been removed)", page)
	}
	// An untitled post still needs a name for its folder, and the post's own
	// id is a better one than whatever the URL happened to spell it as.
	return files, util.FirstNonEmpty(strings.TrimSpace(post.Title), post.ID, id), nil
}

// singleMedia resolves an id that is not a post: an image inside somebody's
// album, or one mirrored from a subreddit. Neither has post data of its own,
// and neither states a length anywhere, so this yields one file of unknown
// size — which is what the download-time check after the response headers
// exists for.
func (i *Imgur) singleMedia(ctx context.Context, id string) ([]File, string, error) {
	page := imgurRoot + "/" + url.PathEscape(id)
	doc, err := i.client.GetString(ctx, page, httpx.Referer(imgurRoot+"/"))
	if err != nil {
		return nil, "", fmt.Errorf("imgur: fetch %s: %w", page, err)
	}
	link, err := imgurMediaFrom(doc)
	if err != nil {
		return nil, "", fmt.Errorf("imgur: %s: %w", page, err)
	}
	file := imgurFileOf(link, -1)
	return []File{file}, file.Name, nil
}

// imgurFilesOf turns a post's media list into files.
//
// The list is the only honest count of what a post holds: image_count is 0 for
// a post that is a single file, so a caller that trusted it would resolve such
// posts to nothing at all.
//
// Each link is queued exactly as the site wrote it. The host serves the same
// object under .jpg and .jpeg alike, so tidying one into the other would gain
// nothing and could only ever disagree with the length stated beside it.
func imgurFilesOf(media []imgurMedia) []File {
	seen := make(map[string]bool, len(media))
	files := make([]File, 0, len(media))
	for _, m := range media {
		if m.URL == "" || seen[m.URL] || !imgurUsableLink(m.URL) {
			continue
		}
		seen[m.URL] = true
		files = append(files, imgurFileOf(m.URL, m.Size))
	}
	return files
}

// imgurFileOf builds one file from a media link and the length the site stated
// for it, or -1 when it stated none.
//
// The name is the link's own last segment, which the site writes as
// <media id>.<ext>. Keeping the id is what makes the names distinct — two
// images in one album routinely share an original filename, exactly as on
// pixhost — and taking it from the link rather than rebuilding it from the id
// and extension separately means the name can never disagree with the bytes.
func imgurFileOf(link string, size int64) File {
	if mp4, swapped := imgurPreferMP4(link); swapped {
		// The stated length describes the GIF, and the MP4 is a fraction of
		// it. Reporting it would be worse than reporting nothing: the check
		// that skips a file already on disk would be measuring against a
		// length this transfer can never reach.
		link, size = mp4, -1
	}
	if size <= 0 {
		size = -1
	}
	return File{
		Name:    util.NameFromURL(link),
		URL:     link,
		Size:    size,
		Headers: httpx.Referer(imgurRoot + "/"),
	}
}

// imgurPreferMP4 swaps a GIF for the H.264 rendition the site generates
// alongside every animated upload, and reports whether it did.
//
// Worth doing on size alone: one clip measured 115,434,148 bytes as a GIF and
// 6,165,363 as MP4, and the ratio held across every animation checked. The
// caller has to drop the stated length in exchange, because the post data
// states the GIF's.
func imgurPreferMP4(link string) (string, bool) {
	u, err := url.Parse(link)
	if err != nil || !strings.EqualFold(path.Ext(u.Path), imgurGIFExt) {
		return link, false
	}
	u.Path = strings.TrimSuffix(u.Path, path.Ext(u.Path)) + imgurMP4Ext
	return u.String(), true
}

// imgurUsableLink reports whether a media link is worth queueing at all.
//
// A file that has been removed is not answered with an error. The media host
// redirects to a fixed 503-byte placeholder and serves it properly: image/png,
// an honest Content-Length, and 206 to a Range request. The downloader's guard
// against a host that answers with a web page instead of bytes returns early
// on a 206, so nothing downstream can catch this one — the placeholder would
// land on disk and be recorded as a finished download. Refusing the path is
// the only place it can be refused.
func imgurUsableLink(link string) bool {
	u, err := url.Parse(link)
	return err == nil && u.Path != imgurRemovedPath
}

// ----------------------------------------------------------------- listings

// listing resolves an account, a tag or a mirrored subreddit.
func (i *Imgur) listing(ctx context.Context, target imgurTarget) (*Result, error) {
	entries, err := i.listingEntries(ctx, target.Listing)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("imgur: %s%s lists no posts", imgurRoot, target.Listing)
	}
	files := i.expand(ctx, entries)
	if len(files) == 0 {
		return nil, fmt.Errorf("imgur: none of the %d posts on %s%s could be read",
			len(entries), imgurRoot, target.Listing)
	}
	return &Result{Title: target.Title, Files: files}, nil
}

// listingEntries walks a listing's pages.
//
// A listing that does not exist is not reported as absent, because the site
// does not answer it as absent: it serves its ordinary HTML shell under a 200,
// so the JSON decode failing on the first page is what "no such account or
// tag" looks like, and the decode error carries the shell's opening bytes to
// say so. A later page failing leaves the posts already collected intact,
// which is worth more than the few at the end.
func (i *Imgur) listingEntries(ctx context.Context, base string) ([]imgurEntry, error) {
	walk := imgurPageWalk{seen: make(map[string]bool)}

	for page := 0; page < config.MaxAlbumPages; page++ {
		endpoint := fmt.Sprintf("%s%s/page/%d/hit.json?scrolled", imgurRoot, base, page)

		var body struct {
			Data []imgurEntry `json:"data"`
		}
		if err := i.client.GetJSON(ctx, endpoint, httpx.Referer(imgurRoot+"/"), &body); err != nil {
			if page == 0 {
				return nil, fmt.Errorf("imgur: fetch %s: %w", endpoint, err)
			}
			break
		}
		if !walk.add(body.Data) {
			break
		}
	}
	return walk.entries, nil
}

// imgurPageWalk accumulates a listing's rows and decides when it has ended.
type imgurPageWalk struct {
	entries []imgurEntry
	seen    map[string]bool
}

// add folds one page in and reports whether another is worth asking for.
//
// Three ways to stop, because the listings disagree about what running past
// the end means. An account's stops answering, so the array comes back empty.
// A mirrored subreddit's answers with its first page again, and goes on doing
// so forever, so a page carrying no id that has not already been seen is the
// end too. A tag's does neither: it keeps handing back older posts for as long
// as it is asked, which is what the count bound is for.
func (w *imgurPageWalk) add(page []imgurEntry) bool {
	added := 0
	for _, entry := range page {
		if entry.Hash == "" || w.seen[entry.Hash] {
			continue
		}
		w.seen[entry.Hash] = true
		w.entries = append(w.entries, entry)
		added++
	}
	return added > 0 && len(w.entries) < config.MaxListingPosts
}

// expand turns listing rows into files.
//
// A row that is itself the media costs nothing and is built here. A row naming
// a post needs that post's page, so those are fetched several at a time, with
// the results collected by index rather than appended as they arrive: the job
// then lists its files in the listing's own order however the requests
// interleave. One post that will not load is skipped, because on a listing of
// hundreds the rest are still worth having.
func (i *Imgur) expand(ctx context.Context, entries []imgurEntry) []File {
	groups := make([][]File, len(entries))

	var wg sync.WaitGroup
	slots := make(chan struct{}, config.PageFetchConcurrency)
	for n, entry := range entries {
		if file, ok := entry.media(); ok {
			groups[n] = []File{file}
			continue
		}
		wg.Add(1)
		go func(n int, id string) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			if files, _, err := i.postFiles(ctx, id); err == nil {
				groups[n] = files
			}
		}(n, entry.Hash)
	}
	wg.Wait()

	var files []File
	for _, group := range groups {
		files = append(files, group...)
	}
	return files
}

// media builds the file for a row that is the media rather than a post, and
// reports whether the row is one.
//
// These rows name no URL, so this is the one place a link is assembled from an
// id and an extension rather than copied. What makes that safe is that the
// same row states the extension and, usually, the exact byte length of what
// the link serves — one record agreeing with itself, not a guess about how the
// host spells things. A row marked an album states neither and names only the
// post, whose page has to be read.
func (e *imgurEntry) media() (File, bool) {
	ext := strings.TrimSpace(e.Ext)
	if e.IsAlbum || e.Hash == "" || ext == "" {
		return File{}, false
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return imgurFileOf("https://"+imgurMediaHost+"/"+url.PathEscape(e.Hash)+ext, e.Size), true
}

// ------------------------------------------------------------------ parsing

// imgurPostFrom pulls the post object out of a page.
//
// The value is nested twice, and the outer layer is not what it looks like: it
// is a *JavaScript* string literal, not a JSON one. Everything inside is
// escaped as JSON would escape it save for one thing — an apostrophe is
// written \', which JSON rejects outright, and which roughly a quarter of
// posts carry in their title or a caption. So the literal is scanned out
// escape-aware and that single backslash dropped, after which the standard
// decoder handles the rest exactly, surrogate pairs and \u escapes included.
// The inner layer is then an ordinary JSON document.
func imgurPostFrom(doc string) (*imgurPost, error) {
	literal, err := imgurPostLiteral(doc)
	if err != nil {
		return nil, err
	}
	var nested string
	if err := json.Unmarshal([]byte(literal), &nested); err != nil {
		return nil, fmt.Errorf("%s is not a readable string: %w", imgurPostData, err)
	}
	var post imgurPost
	if err := json.Unmarshal([]byte(nested), &post); err != nil {
		return nil, fmt.Errorf("%s does not hold a post: %w", imgurPostData, err)
	}
	return &post, nil
}

// imgurPostLiteral scans out the assigned string literal, rewriting the one
// escape JavaScript allows and JSON does not.
//
// The scan has to be escape-aware anyway, since only that tells which quote
// ends the literal, so turning \' into ' along the way costs nothing — and
// leaves a value encoding/json can decode, rather than an unescaper of our own
// that would have to get \uXXXX and its surrogate pairs right to match it.
//
// Only the global's absence yields errNoImgurPost. A page that names it and
// then carries something unreadable is a change in the site or a regression
// here, and reporting that as "this id holds nothing" would send whoever looks
// into it in exactly the wrong direction.
func imgurPostLiteral(doc string) (string, error) {
	_, rest, ok := strings.Cut(doc, imgurPostData)
	if !ok {
		return "", errNoImgurPost
	}
	// Only the assignment may stand between the name and the value, so a
	// mention of it anywhere else on the page is not mistaken for one.
	rest = strings.TrimLeft(rest, " \t\r\n=")
	if rest == "" || rest[0] != '"' {
		return "", fmt.Errorf("%s is not assigned a string", imgurPostData)
	}

	const unterminated = "%s is assigned a string that never ends"

	var b strings.Builder
	b.Grow(len(rest))
	b.WriteByte('"')
	for n := 1; n < len(rest); n++ {
		switch c := rest[n]; c {
		case '\\':
			if n+1 == len(rest) {
				return "", fmt.Errorf(unterminated, imgurPostData)
			}
			n++
			if rest[n] != '\'' {
				b.WriteByte('\\')
			}
			b.WriteByte(rest[n])
		case '"':
			b.WriteByte('"')
			return b.String(), nil
		default:
			b.WriteByte(c)
		}
	}
	return "", fmt.Errorf(unterminated, imgurPostData)
}

// imgurMediaFrom reads the one file a bare media page shows.
//
// og:image names it; twitter:image does not, and the difference is invisible
// unless you know what to look for. The site encodes a thumbnail size as a
// letter appended to the id, and the card image is a thumbnail written that
// way — 68,297 bytes against the original's 2,267,304, and a different format
// besides. A loose search for an i.imgur.com link anywhere on the page would
// find that one too, so only the tag naming the original is read.
//
// Requiring the media host is what keeps the site's own pages out. Its
// server-rendered listings are perfectly ordinary pages carrying an og:image
// of the imgur logo, and without the check a mis-pasted /hot or /search would
// resolve to it and download happily.
//
// The query string goes. The site tags the link for whichever crawler it was
// written for — ?fb, ?fbplay, ?noredirect — and it is served identically
// without.
func imgurMediaFrom(doc string) (string, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return "", err
	}
	raw := metaContent(root, "og:image")
	if raw == "" {
		return "", errors.New("no media on the page (it may have been removed)")
	}
	u, err := url.Parse(raw)
	if err != nil || !util.HostMatches(u.Host, imgurMediaHost) {
		return "", errors.New("the page shows no file of its own")
	}
	u.RawQuery = ""
	u.Fragment = ""
	if !imgurUsableLink(u.String()) {
		return "", errors.New("the file has been removed")
	}
	return u.String(), nil
}

// imgurPost is the object a post page carries, and the one the API returned
// when it still answered. Its shape is the same for an album, for a gallery
// post holding a single file, and for a listing's expansion, which is why
// there is only one of it.
type imgurPost struct {
	ID    string       `json:"id"`
	Title string       `json:"title"`
	Media []imgurMedia `json:"media"`
}

// imgurMedia is one file in a post. Only the link and the length are read: the
// name follows from the link, and everything else the object carries — the
// dimensions, the caption, the animation flags — describes the file rather
// than saying where it is.
type imgurMedia struct {
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

// imgurEntry is one row of a listing.
type imgurEntry struct {
	// Hash is a post id for an album row and a media id otherwise.
	Hash string `json:"hash"`
	// IsAlbum marks a row that names a post rather than a file.
	IsAlbum bool `json:"is_album"`
	// Size is the exact length of the file a media row names, and 0 on an
	// album row, which names no file.
	Size int64 `json:"size"`
	// Ext carries the leading dot, ".mp4".
	Ext string `json:"ext"`
}
