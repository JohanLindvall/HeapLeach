package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Civitai publishes a documented JSON API over what its users post, and it is
// the plainest host here: the listing hands out the original file's URL in a
// field, the CDN is content-addressed and unsigned, and a range request
// answers 206 with a correct Content-Range. Nothing is signed, expires or
// needs a session, so there is no resolver, no cipher and no playlist — a
// page of items becomes a list of files and that is the whole extractor.
//
// The care all goes into the query, because three of the parameters an
// obvious implementation would reach for are wrong in ways the response never
// admits to:
//
//   - nsfw=X is a correctness requirement rather than a preference. Left off,
//     the API quietly applies the browsing level of a signed-out visitor:
//     across a sample of twenty-five posts that hid 37 images of 95, and
//     emptied several posts completely. The gallery that came back would look
//     complete, which is the worst way to be wrong.
//   - collectionId is accepted and then ignored. Asked with a deliberately
//     nonexistent id it answers with the same images a bare query does — the
//     site's front page. A /collections/<id> shape built on it would hand the
//     user the front page while reporting a collection downloaded, so that
//     shape is not matched at all and falls through to the direct extractor,
//     which fails honestly.
//   - modelId is accepted and returns nothing: zero items for four of the top
//     five models. A model's images are nested under its versions at
//     /api/v1/models/<id>, and that is where they are read from instead.
//
// The plural selectors imageIds= and ids= are ignored the same silent way;
// the singular imageId= is the one that works.
//
// One field lies quietly. The API writes every image URL as .jpeg whatever
// was uploaded, and two in five are really PNG — the CDN's Content-Type and
// Content-Disposition both say so, only the URL does not. The saved name
// still follows the URL, because a name left without an extension would be
// replaced wholesale by the one the CDN offers, which is the storage UUID:
// the right suffix on a filename nobody can match against the page they
// copied it from.
type Civitai struct {
	client *httpx.Client
}

const (
	civitaiRoot = "https://civitai.com"
	civitaiAPI  = civitaiRoot + "/api/v1"

	// civitaiPageSize is the listing's own maximum page.
	civitaiPageSize = 100

	// civitaiNSFW lifts mature filtering for a caller with no account. It
	// names a ceiling on the browsing level rather than selecting one, so
	// everything below it still comes back.
	civitaiNSFW = "X"
)

// The selectors the image listing honours. Exactly one is sent per query;
// combining them narrows nothing, and the ones left out are ignored rather
// than refused.
const (
	civitaiByImage = "imageId"
	civitaiByPost  = "postId"
	civitaiByUser  = "username"
)

// civitaiUserTabs are the profile tabs that are the same gallery seen from a
// different angle. A profile's model list is not, and is left unmatched
// rather than answered with the user's own pictures.
var civitaiUserTabs = map[string]bool{"images": true, "posts": true, "videos": true}

// NewCivitai builds the civitai extractor.
func NewCivitai(client *httpx.Client) *Civitai { return &Civitai{client: client} }

func (c *Civitai) Name() string { return "civitai" }

func (c *Civitai) Match(u *url.URL) bool { return civitaiTargetOf(u) != nil }

// civitaiTarget is what one input URL asks for.
type civitaiTarget struct {
	// selector is the listing parameter to send, or "" when value is a
	// model id and the other endpoint is wanted.
	selector string
	value    string
}

// civitaiTargetOf reads what a page URL asks for, and returns nothing for
// every other shape on the site — /collections/<id> deliberately among them,
// and /api/download/... too, so a model file link still reaches the direct
// extractor that can fetch it.
func civitaiTargetOf(u *url.URL) *civitaiTarget {
	if !util.HostMatches(u.Host, "civitai.com") {
		return nil
	}
	segs := util.PathSegments(u)
	if len(segs) < 2 {
		return nil
	}
	switch segs[0] {
	case "images":
		if civitaiIsID(segs[1]) {
			return &civitaiTarget{selector: civitaiByImage, value: segs[1]}
		}
	case "posts":
		if civitaiIsID(segs[1]) {
			return &civitaiTarget{selector: civitaiByPost, value: segs[1]}
		}
	case "models":
		if civitaiIsID(segs[1]) {
			return &civitaiTarget{value: segs[1]}
		}
	case "user":
		if len(segs) == 2 || civitaiUserTabs[segs[2]] {
			return &civitaiTarget{selector: civitaiByUser, value: segs[1]}
		}
	}
	return nil
}

// civitaiIsID reports whether a path segment is one of the site's numeric
// ids. That is what separates /models/<id> from /models/train.
func civitaiIsID(seg string) bool {
	_, err := strconv.ParseUint(seg, 10, 64)
	return err == nil
}

// Extract resolves an image, a post, a user's gallery or a model.
func (c *Civitai) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	target := civitaiTargetOf(u)
	if target == nil {
		return nil, fmt.Errorf("civitai: %s is not an image (/images/<id>), a post "+
			"(/posts/<id>), a model (/models/<id>) or a user gallery (/user/<name>)", u.Redacted())
	}
	if target.selector == "" {
		return c.model(ctx, target.value)
	}

	items, err := c.listing(ctx, target)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("civitai: %s lists no images (it may have been removed or made private)",
			u.Redacted())
	}

	files := make([]File, 0, len(items))
	for _, item := range items {
		files = append(files, civitaiFile(item))
	}
	return &Result{Title: civitaiTitle(target, items), Files: files}, nil
}

// listing collects everything one selector names.
func (c *Civitai) listing(ctx context.Context, target *civitaiTarget) ([]civitaiImage, error) {
	return c.walk(ctx, civitaiQuery(civitaiAPI+"/images", target))
}

// civitaiQuery builds the first page's request: the site's own maximum page,
// the browsing level that makes the answer complete, and exactly one
// selector.
func civitaiQuery(endpoint string, target *civitaiTarget) string {
	query := url.Values{}
	query.Set("limit", strconv.Itoa(civitaiPageSize))
	query.Set("nsfw", civitaiNSFW)
	query.Set(target.selector, target.value)
	return endpoint + "?" + query.Encode()
}

// walk follows the listing's cursor from one page to the next. The page count
// is bounded the way every album listing here is, as a backstop against a site
// that keeps answering rather than ending.
func (c *Civitai) walk(ctx context.Context, next string) ([]civitaiImage, error) {
	var (
		items []civitaiImage
		seen  = make(map[int64]bool)
	)
	for page := 0; next != "" && page < config.MaxAlbumPages; page++ {
		payload, err := c.page(ctx, next)
		if err != nil {
			return nil, err
		}
		if len(payload.Items) == 0 {
			break
		}
		for _, item := range payload.Items {
			// A cursor boundary hands the same item out twice, once at the
			// end of a page and again at the head of the next.
			if item.URL == "" || seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			items = append(items, item)
		}
		next = civitaiNextPage(next, payload.Metadata.NextPage)
	}
	return items, nil
}

// civitaiNextPage takes the cursor link the listing supplies as it stands.
// It is absolute and already carries the selector, the page size and the
// browsing level, so rebuilding it around the raw cursor would be a guess at
// a format the site owns. All that is checked is that it stays on the host we
// were already talking to, so a listing cannot walk the paging loop off it.
func civitaiNextPage(current, raw string) string {
	if raw == "" {
		return ""
	}
	next, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	here, err := url.Parse(current)
	if err != nil || !strings.EqualFold(next.Host, here.Host) {
		return ""
	}
	return raw
}

// page fetches one listing page, waiting out the overload the search backend
// answers with while it is busy.
//
// That answer is a 503 saying the image search is temporarily overloaded, and
// it arrives with neither Retry-After nor any rate-limit header to pace by,
// so the wait is our own. What it must never be read as is the end of the
// listing: a gallery cut short at page seven looks exactly like a finished
// one, so an overload that outlasts the attempts fails the extraction rather
// than quietly returning most of it.
func (c *Civitai) page(ctx context.Context, endpoint string) (*civitaiListing, error) {
	var last error
	for attempt := range config.ExtractRetries {
		if attempt > 0 {
			wait := util.Backoff(attempt-1, config.RequestRetryBase, config.RequestRetryMax)
			if err := util.SleepCtx(ctx, wait); err != nil {
				return nil, err
			}
		}
		var payload civitaiListing
		err := c.client.GetJSON(ctx, endpoint, httpx.Referer(civitaiRoot+"/"), &payload)
		if err == nil {
			return &payload, nil
		}
		last = err
		if !httpx.HasStatus(err, http.StatusServiceUnavailable) {
			return nil, fmt.Errorf("civitai: %w", err)
		}
	}
	return nil, fmt.Errorf("civitai: the image search stayed overloaded, so this listing "+
		"is incomplete rather than empty — try again shortly: %w", last)
}

// model reads a model's showcase images, which the image listing cannot
// reach: its modelId parameter is accepted and answers with nothing.
func (c *Civitai) model(ctx context.Context, id string) (*Result, error) {
	endpoint := civitaiAPI + "/models/" + url.PathEscape(id)

	var payload civitaiModel
	if err := c.client.GetJSON(ctx, endpoint, httpx.Referer(civitaiRoot+"/"), &payload); err != nil {
		return nil, fmt.Errorf("civitai: model %s: %w", id, err)
	}
	files := civitaiModelFiles(&payload)
	if len(files) == 0 {
		return nil, fmt.Errorf("civitai: model %s shows no images", id)
	}
	return &Result{Title: util.FirstNonEmpty(payload.Name, "civitai-model-"+id), Files: files}, nil
}

// civitaiModelFiles flattens a model's versions, filing each version's
// showcase into its own folder the way the page groups them. An image two
// versions both offer is downloaded once, under the earlier of them.
func civitaiModelFiles(model *civitaiModel) []File {
	seen := make(map[string]bool)
	var files []File
	for _, version := range model.Versions {
		dir := folderName(version.Name)
		for _, img := range version.Images {
			if img.URL == "" || seen[img.URL] {
				continue
			}
			seen[img.URL] = true
			file := civitaiFile(img)
			file.Dir = dir
			files = append(files, file)
		}
	}
	return files
}

// civitaiFile turns one item into a download. The URL is taken exactly as
// given, /original=true/ segment included: the same asset under /width=450/
// is a tenth of the size, and it is the transform, not the file, that the
// path segment selects.
func civitaiFile(img civitaiImage) File {
	return File{
		Name:    civitaiName(img),
		URL:     img.URL,
		Size:    -1,
		Headers: httpx.Referer(civitaiRoot + "/"),
	}
}

// civitaiName names a file after the image's own id, which is what the site
// keys /images/<id> on, so a saved file can be traced back to the page it
// came from. Nothing better is on offer: the API publishes no original
// filename, and the CDN path's last segment identifies nothing — swap it for
// any other name and the same bytes still arrive.
//
// A model version's images carry no id field at all. There the last path
// segment is the image id rather than the storage UUID the listing writes, so
// it serves as the name unchanged; and if that ever stops being true the name
// is merely opaque rather than wrong.
func civitaiName(img civitaiImage) string {
	name := util.NameFromURL(img.URL)
	if img.ID > 0 {
		return strconv.FormatInt(img.ID, 10) + path.Ext(name)
	}
	return name
}

// civitaiTitle names the job. The listing carries the poster's name in its
// own casing, which is worth preferring over whatever casing the input URL
// happened to use.
func civitaiTitle(target *civitaiTarget, items []civitaiImage) string {
	who := ""
	if len(items) > 0 {
		who = items[0].Username
	}
	switch target.selector {
	case civitaiByUser:
		return util.FirstNonEmpty(who, target.value)
	case civitaiByPost:
		if who != "" {
			return who + " post " + target.value
		}
		return "civitai-post-" + target.value
	default:
		if who != "" {
			return who + " " + target.value
		}
		return "civitai-" + target.value
	}
}

// civitaiListing is one page of the image endpoint.
type civitaiListing struct {
	Items    []civitaiImage `json:"items"`
	Metadata struct {
		NextPage string `json:"nextPage"`
	} `json:"metadata"`
}

// civitaiImage is one item. The listing fills all of it; a model version's
// images carry the URL and nothing else this needs.
type civitaiImage struct {
	ID       int64  `json:"id"`
	URL      string `json:"url"`
	Username string `json:"username"`
}

// civitaiModel is the model endpoint's payload, cut down to the showcase.
type civitaiModel struct {
	Name     string `json:"name"`
	Versions []struct {
		Name   string         `json:"name"`
		Images []civitaiImage `json:"images"`
	} `json:"modelVersions"`
}
