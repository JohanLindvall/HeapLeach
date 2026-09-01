package extractor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// DrTuber resolves a video through the player's own configuration endpoint.
//
// The page carries no media at all: its <video> element has a poster and
// nothing else, and everything the player needs — title, duration and every
// rendition — arrives from /player_config_json/ in one request. So that
// endpoint is the whole extractor, and the watch page is never fetched.
//
// One property of it is load-bearing and completely invisible: it answers
// only a request whose Accept names JSON. Asked with the "*/*" an ordinary
// fetch sends, it returns an empty JSON array under a 200, which reads like
// a video that does not exist rather than like a refusal. GetJSON sends the
// Accept that works, and that is the reason to keep using it here rather
// than fetching the body and decoding it by hand — the plain-fetch version
// of this extractor looks correct and never resolves anything.
//
// The links it hands back are signed and carry an expiry a few minutes out,
// so the configuration is read again when the item actually starts.
type DrTuber struct {
	hostSet
	client *httpx.Client
}

const drtuberRoot = "https://www.drtuber.com"

// drtuberID matches the two shapes a video is linked by: the watch page,
// /video/<id>/<slug>, and the player the embeds point at, /embed/<id>.
var drtuberID = regexp.MustCompile(`^/(?:video|embed)/(\d+)`)

// drtuberQualities maps the rendition names the configuration uses to the
// vertical resolutions the player displays for them. They are names rather
// than resolutions, so qualityOf cannot read them, and sorted as text "lq"
// would beat "4k".
var drtuberQualities = map[string]int{"lq": 320, "hq": 720, "4k": 2160}

// NewDrTuber builds the drtuber extractor.
func NewDrTuber(client *httpx.Client) *DrTuber {
	return &DrTuber{hostSet: hostSet{"drtuber.com"}, client: client}
}

func (d *DrTuber) Name() string { return "drtuber" }

// Extract resolves a video page to its best rendition, re-reading the
// configuration at download time because the links expire.
func (d *DrTuber) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	m := drtuberID.FindStringSubmatch(u.Path)
	if m == nil {
		return nil, fmt.Errorf("drtuber: %s names no video", u.Redacted())
	}
	id := m[1]
	return refetchedVideo(ctx, ".mp4", httpx.Referer(drtuberRoot+"/"),
		func(ctx context.Context) (string, string, error) { return d.best(ctx, id) })
}

// best reads the player configuration and picks the largest rendition.
func (d *DrTuber) best(ctx context.Context, id string) (string, string, error) {
	var raw json.RawMessage
	endpoint := drtuberRoot + "/player_config_json/?vid=" + url.QueryEscape(id)
	if err := d.client.GetJSON(ctx, endpoint, nil, &raw); err != nil {
		return "", "", fmt.Errorf("drtuber: read the player configuration for video %s: %w", id, err)
	}

	// A video that is gone, and a request the endpoint declines to answer,
	// both come back as an empty JSON array under a 200 rather than as an
	// error. The shape of the reply is the only thing separating either
	// from a real answer, so it is checked before decoding — otherwise the
	// user is shown a mismatched-type error from the JSON decoder.
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return "", "", fmt.Errorf("drtuber: no player configuration for video %s "+
			"(it has been removed, or is not served to this request)", id)
	}

	var config struct {
		Title string            `json:"title"`
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return "", "", fmt.Errorf("drtuber: read the player configuration for video %s: %w", id, err)
	}

	best, ok := bestCandidate(drtuberRenditions(config.Files))
	if !ok {
		return "", "", fmt.Errorf("drtuber: video %s lists no playable rendition", id)
	}
	return util.FirstNonEmpty(strings.TrimSpace(config.Title), "drtuber-"+id), best.URL, nil
}

// drtuberRenditions turns the configuration's file list into candidates.
//
// The list names every rendition the site knows about whether or not this
// video has one, so the absent ones — an empty string, or a JSON null, which
// decodes to the same — are dropped rather than ranked.
//
// It walks the names in sorted order because a map's iteration order is
// random, and two renditions of equal quality would otherwise be chosen
// differently on each visit. That includes between the resolve at queue time
// and the one at download time, which is where it would be noticed.
func drtuberRenditions(files map[string]string) []mediaCandidate {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)

	var out []mediaCandidate
	for _, name := range names {
		link := files[name]
		if link == "" {
			continue
		}
		quality := drtuberQualities[name]
		if quality == 0 {
			quality = qualityOf(name + " " + link)
		}
		out = append(out, mediaCandidate{
			URL:     link,
			Quality: quality,
			IsHLS:   strings.Contains(link, ".m3u8"),
		})
	}
	return out
}
